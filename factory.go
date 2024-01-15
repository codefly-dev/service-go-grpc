package main

import (
	"context"
	"embed"
	"fmt"

	protohelpers "github.com/codefly-dev/core/agents/helpers/proto"

	"github.com/codefly-dev/core/builders"

	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"

	"github.com/codefly-dev/core/agents/communicate"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	golanghelpers "github.com/codefly-dev/core/agents/helpers/go"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	factoryv0 "github.com/codefly-dev/core/generated/go/services/factory/v0"
)

type Factory struct {
	*Service

	gohelper    *golanghelpers.Go
	protohelper *protohelpers.Proto
}

func NewFactory() *Factory {
	return &Factory{
		Service: NewService(),
	}
}
func (s *Factory) Load(ctx context.Context, req *factoryv0.LoadRequest) (*factoryv0.LoadResponse, error) {
	defer s.Wool.Catch()

	err := s.Base.Factory.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return nil, err
	}

	err = s.LoadEndpoints(ctx)
	if err != nil {
		return s.Factory.LoadError(err)
	}

	// communication on CreateResponse
	err = s.Communication.Register(ctx, communicate.New[factoryv0.CreateRequest](createCommunicate()))
	if err != nil {
		return s.Factory.LoadError(err)
	}

	if err != nil {
		return s.Factory.LoadError(err)
	}

	gettingStarted, err := templates.ApplyTemplateFrom(shared.Embed(factory), "templates/factory/GETTING_STARTED.md", s.Information)
	if err != nil {
		return s.Factory.LoadError(err)
	}
	return s.Factory.LoadResponse(s.Endpoints, gettingStarted)
}

func (s *Factory) Init(ctx context.Context, req *factoryv0.InitRequest) (*factoryv0.InitResponse, error) {
	defer s.Wool.Catch()

	hash, err := requirements.Hash(ctx)
	if err != nil {
		return s.Factory.InitError(err)
	}

	return s.Factory.InitResponse(hash)
}

const Watch = "with-hot-reload"
const WithDebugSymbols = "with-debug-symbols"
const WithRest = "create-rest-endpoint"

func createCommunicate() *communicate.Sequence {
	return communicate.NewSequence(
		communicate.NewConfirm(&agentv0.Message{Name: Watch, Message: "Code hot-reload (Recommended)?", Description: "codefly can restart your service when code changes are detected 🔎"}, true),
		communicate.NewConfirm(&agentv0.Message{Name: WithRest, Message: "Automatic REST generation (Recommended)?", Description: "codefly can generate a REST server that stays magically 🪄 synced to your gRPC defLoadion -- the easiest way to do REST"}, true),
		communicate.NewConfirm(&agentv0.Message{Name: WithDebugSymbols, Message: "Start with debug symbols?", Description: "Build the go binary with debug symbol to use stack debugging"}, true),
	)
}

type CreateConfiguration struct {
	*services.Information
	Image *configurations.DockerImage
	Envs  []string
}

func (s *Factory) Create(ctx context.Context, req *factoryv0.CreateRequest) (*factoryv0.CreateResponse, error) {
	defer s.Wool.Catch()

	ctx = s.WoolAgent.Inject(ctx)

	session, err := s.Communication.Done(ctx, communicate.Channel[factoryv0.CreateRequest]())
	if err != nil {
		return s.Factory.CreateError(err)
	}

	s.Settings.Watch, err = session.Confirm(Watch)
	if err != nil {
		return s.Factory.CreateError(err)
	}

	s.Settings.CreateHttpEndpoint, err = session.Confirm(WithRest)
	if err != nil {
		return s.Factory.CreateError(err)
	}

	s.Settings.WithDebugSymbols, err = session.Confirm(WithDebugSymbols)
	if err != nil {
		return s.Factory.CreateError(err)
	}

	create := CreateConfiguration{
		Information: s.Information,
		Image:       s.DockerImage(),
		Envs:        []string{},
	}
	ignores := []string{"go.work", "service.generation.codefly.yaml"}
	err = s.Templates(ctx, create, services.WithFactory(factory, ignores...))
	if err != nil {
		return s.Base.Factory.CreateError(err)
	}

	s.gohelper = &golanghelpers.Go{Dir: s.Location}
	err = s.gohelper.ModTidy(ctx)
	if err != nil {
		return s.Base.Factory.CreateError(err)
	}

	s.protohelper, err = protohelpers.NewProto(ctx, s.Local("proto"))
	if err != nil {
		return s.Base.Factory.CreateError(err)
	}
	err = s.protohelper.Generate(ctx)
	if err != nil {
		return s.Base.Factory.CreateError(err)
	}

	err = s.CreateEndpoints(ctx)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create endpoints")
	}

	return s.Base.Factory.CreateResponse(ctx, s.Settings, s.Endpoints...)
}

func (s *Factory) Update(ctx context.Context, req *factoryv0.UpdateRequest) (*factoryv0.UpdateResponse, error) {
	defer s.Wool.Catch()

	err := s.Base.Templates(nil, services.WithBuilder(builder))
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot copy and apply template")
	}

	err = s.gohelper.Update(ctx)
	if err != nil {
		return nil, fmt.Errorf("factory>update: go helper: cannot run update: %v", err)
	}
	return &factoryv0.UpdateResponse{}, nil
}

func (s *Factory) Sync(ctx context.Context, req *factoryv0.SyncRequest) (*factoryv0.SyncResponse, error) {
	defer s.Wool.Catch()

	// err := os.RemoveAll(s.Local("adapters/servicev0"))
	// if err != nil {
	// 	return nil, s.Wool.Wrapf(err, "cannot remove adapters")
	// }
	// // Re-generate
	// s.Wool.TODO("change buf to use openapi or not depending on things...")

	// err = s.protohelper.Generate(ctx)
	// if err != nil {
	// 	return nil, s.Wool.Wrapf(err, "cannot generate proto")
	// }

	// err = s.gohelper.ModTidy(ctx)
	// if err != nil {
	// 	return nil, s.Wool.Wrapf(err, "cannot tidy go.mod")
	// }

	return &factoryv0.SyncResponse{}, nil
}

type Env struct {
	Key   string
	Value string
}

type DockerTemplating struct {
	Dependency builders.Dependency
	Envs       []Env
}

func (s *Factory) Build(ctx context.Context, req *factoryv0.BuildRequest) (*factoryv0.BuildResponse, error) {
	s.Wool.Debug("building docker image")
	ctx = s.WoolAgent.Inject(ctx)

	docker := DockerTemplating{
		Dependency: *requirements,
	}

	endpoint := configurations.FromProtoEndpoint(s.GrpcEndpoint)
	gRPC := configurations.EndpointEnvironmentVariableKey(endpoint)
	docker.Envs = append(docker.Envs, Env{Key: gRPC, Value: "localhost:9090"})

	if s.RestEndpoint != nil {
		endpoint = configurations.FromProtoEndpoint(s.RestEndpoint)
		rest := configurations.EndpointEnvironmentVariableKey(endpoint)
		docker.Envs = append(docker.Envs, Env{Key: rest, Value: "localhost:8080"})
	}

	err := shared.DeleteFile(ctx, s.Local("codefly/builder/Dockerfile"))
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot remove dockerfile")
	}
	err = s.Templates(ctx, docker, services.WithBuilder(builder))
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot copy and apply template")
	}
	image := s.DockerImage()
	builder, err := dockerhelpers.NewBuilder(dockerhelpers.BuilderConfiguration{
		Root:        s.Location,
		Dockerfile:  "codefly/builder/Dockerfile",
		Destination: image,
	})
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create builder")
	}
	_, err = builder.Build(ctx)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot build image")
	}
	return &factoryv0.BuildResponse{}, nil
}

type Deployment struct {
	Replicas int
}

type DeploymentParameter struct {
	Image *configurations.DockerImage
	*services.Information
	Deployment
}

func (s *Factory) Deploy(ctx context.Context, req *factoryv0.DeploymentRequest) (*factoryv0.DeploymentResponse, error) {
	defer s.Wool.Catch()
	deploy := DeploymentParameter{Image: s.DockerImage(), Information: s.Information, Deployment: Deployment{Replicas: 1}}
	err := s.Templates(ctx, deploy) //services.WithDeploymentFor(deployment, "kustomize/base", templates.WithOverrideAll()),
	//services.WithDeploymentFor(deployment, "kustomize/overlays/environment",
	//	services.WithDestination("kustomize/overlays/%s", req.Environment.Name), templates.WithOverrideAll()),

	if err != nil {
		return nil, err
	}
	return &factoryv0.DeploymentResponse{}, nil
}

func (s *Factory) CreateEndpoints(ctx context.Context) error {
	grpc, err := configurations.NewGrpcAPI(ctx, &configurations.Endpoint{Name: "grpc"}, s.Local("proto/api.proto"))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create grpc api")
	}
	s.Endpoints = append(s.Endpoints, grpc)

	if s.Settings.CreateHttpEndpoint {
		rest, err := configurations.NewRestAPIFromOpenAPI(ctx, &configurations.Endpoint{Name: "rest", Visibility: "private"}, s.Local("proto/swagger/api.swagger.json"))
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create openapi api")
		}
		s.Endpoints = append(s.Endpoints, rest)
	}
	return nil
}

//go:embed templates/factory
var factory embed.FS

//go:embed templates/builder
var builder embed.FS

//go:embed templates/deployment
var deployment embed.FS
