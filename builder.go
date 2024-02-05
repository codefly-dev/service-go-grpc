package main

import (
	"context"
	"embed"
	"fmt"

	protohelpers "github.com/codefly-dev/core/agents/helpers/proto"

	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"

	"github.com/codefly-dev/core/agents/communicate"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	golanghelpers "github.com/codefly-dev/core/agents/helpers/go"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
)

type Builder struct {
	*Service

	gohelper    *golanghelpers.Go
	protohelper *protohelpers.Proto
}

func NewBuilder() *Builder {
	return &Builder{
		Service: NewService(),
	}
}
func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()

	err := s.Base.Builder.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return nil, err
	}

	requirements.Localize(s.Location)

	err = s.LoadEndpoints(ctx, false)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	// communication on CreateResponse
	err = s.Communication.Register(ctx, communicate.New[builderv0.CreateRequest](createCommunicate()))
	if err != nil {
		return s.Builder.LoadError(err)
	}

	if err != nil {
		return s.Builder.LoadError(err)
	}

	gettingStarted, err := templates.ApplyTemplateFrom(ctx, shared.Embed(factoryFS), "templates/factory/GETTING_STARTED.md", s.Information)
	if err != nil {
		return s.Builder.LoadError(err)
	}
	return s.Builder.LoadResponse(gettingStarted)
}

func (s *Builder) Init(ctx context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	defer s.Wool.Catch()

	hash, err := requirements.Hash(ctx)
	if err != nil {
		return s.Builder.InitError(err)
	}

	return s.Builder.InitResponse(hash)
}

func (s *Builder) Update(ctx context.Context, req *builderv0.UpdateRequest) (*builderv0.UpdateResponse, error) {
	defer s.Wool.Catch()

	err := s.gohelper.Update(ctx)
	if err != nil {
		return nil, fmt.Errorf("builder>update: go helper: cannot run update: %v", err)
	}
	return &builderv0.UpdateResponse{}, nil
}

func (s *Builder) Sync(ctx context.Context, req *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
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

	return s.Builder.SyncResponse()
}

type Env struct {
	Key   string
	Value string
}

type DockerTemplating struct {
	Components []string
	Envs       []Env
}

func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	s.Wool.Debug("building docker image")
	ctx = s.WoolAgent.Inject(ctx)

	docker := DockerTemplating{
		Components: requirements.All(),
	}

	endpoint := configurations.EndpointFromProto(s.GrpcEndpoint)
	gRPC := configurations.EndpointEnvironmentVariableKey(endpoint)
	docker.Envs = append(docker.Envs, Env{Key: gRPC, Value: "localhost:9090"})

	if s.RestEndpoint != nil {
		endpoint = configurations.EndpointFromProto(s.RestEndpoint)
		rest := configurations.EndpointEnvironmentVariableKey(endpoint)
		docker.Envs = append(docker.Envs, Env{Key: rest, Value: "localhost:8080"})
	}

	err := shared.DeleteFile(ctx, s.Local("codefly/builder/Dockerfile"))
	if err != nil {
		return s.Builder.BuildError(err)
	}

	err = s.Templates(ctx, docker, services.WithBuilder(builderFS))
	if err != nil {
		return s.Builder.BuildError(err)
	}

	image := s.DockerImage()
	builder, err := dockerhelpers.NewBuilder(dockerhelpers.BuilderConfiguration{
		Root:        s.Location,
		Dockerfile:  "codefly/builder/Dockerfile",
		Destination: image,
		Output:      s.Wool,
	})
	if err != nil {
		return s.Builder.BuildError(err)
	}
	_, err = builder.Build(ctx)
	if err != nil {
		return s.Builder.BuildError(err)
	}
	return s.Builder.BuildResponse()
}

type Deployment struct {
	Replicas int
}

type DeploymentParameter struct {
	Image *configurations.DockerImage
	*services.Information
	Deployment
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()
	deploy := DeploymentParameter{Image: s.DockerImage(), Information: s.Information, Deployment: Deployment{Replicas: 1}}
	err := s.Templates(ctx, deploy)
	//services.WithDeploymentFor(deployment, "kustomize/base", templates.WithOverrideAll()),
	//services.WithDeploymentFor(deployment, "kustomize/overlays/environment",
	//	services.WithDestination("kustomize/overlays/%s", req.Environment.Name), templates.WithOverrideAll()),
	//
	if err != nil {
		return nil, err
	}
	return &builderv0.DeploymentResponse{}, nil
}

func (s *Builder) CreateEndpoints(ctx context.Context) error {
	grpc, err := configurations.NewGrpcAPI(ctx, &configurations.Endpoint{Name: "grpc"}, s.Local("proto/api.proto"))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create grpc api")
	}
	s.Endpoints = append(s.Endpoints, grpc)

	if s.Settings.WithRestEndpoint {
		rest, err := configurations.NewRestAPIFromOpenAPI(ctx, &configurations.Endpoint{Name: "rest", Visibility: "private"}, s.Local("proto/swagger/api.swagger.json"))
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create openapi api")
		}
		s.Endpoints = append(s.Endpoints, rest)
	}
	return nil
}

const WithRestEndpoint = "with-rest-endpoint"
const Watch = "with-hot-reload"
const WithDebugSymbols = "with-debug-symbols"
const WithRaceConditionDetectionRun = "with-race-condition-detection-run"

func createCommunicate() *communicate.Sequence {
	return communicate.NewSequence(
		communicate.NewConfirm(&agentv0.Message{Name: Watch, Message: "Code hot-reload (Recommended)?", Description: "codefly can restart your service when code changes are detected 🔎"}, true),
		communicate.NewConfirm(&agentv0.Message{Name: WithRestEndpoint, Message: "Automatic REST generation (Recommended)?", Description: "codefly can generate a REST server that stays magically 🪄 synced to your gRPC definition -- the easiest way to do REST"}, true),
		communicate.NewConfirm(&agentv0.Message{Name: WithDebugSymbols, Message: "Start with debug symbols?", Description: "Build the go binary with debug symbol to use stack debugging"}, true),
		communicate.NewConfirm(&agentv0.Message{Name: WithRaceConditionDetectionRun, Message: "Start with race condition detection?", Description: "Build the go binary with race condition detection"}, true),
	)
}

type CreateConfiguration struct {
	*services.Information
	Image *configurations.DockerImage
	Envs  []string
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()

	ctx = s.WoolAgent.Inject(ctx)

	session, err := s.Communication.Done(ctx, communicate.Channel[builderv0.CreateRequest]())
	if err != nil {
		return s.Builder.CreateError(err)
	}

	s.Settings.WithRestEndpoint, err = session.Confirm(WithRestEndpoint)
	if err != nil {
		return s.Builder.CreateError(err)
	}

	s.Settings.Watch, err = session.Confirm(Watch)
	if err != nil {
		return s.Builder.CreateError(err)
	}

	s.Settings.WithDebugSymbols, err = session.Confirm(WithDebugSymbols)
	if err != nil {
		return s.Builder.CreateError(err)
	}

	s.Settings.WithRaceConditionDetectionRun, err = session.Confirm(WithRaceConditionDetectionRun)
	if err != nil {
		return s.Builder.CreateError(err)
	}

	create := CreateConfiguration{
		Information: s.Information,
		Image:       s.DockerImage(),
		Envs:        []string{},
	}
	ignore := shared.NewIgnore("go.work*", "service.generation.codefly.yaml")
	override := shared.OverrideException(shared.NewIgnore("*.proto")) // Don't override proto

	err = s.Templates(ctx, create, services.WithFactory(factoryFS).WithPathSelect(ignore).WithOverride(override))
	if err != nil {
		return s.Base.Builder.CreateError(err)
	}

	s.gohelper = &golanghelpers.Go{Dir: s.Location}
	err = s.gohelper.ModTidy(ctx)
	if err != nil {
		return s.Base.Builder.CreateError(err)
	}

	s.protohelper, err = protohelpers.NewProto(ctx, s.Location)
	if err != nil {
		return s.Base.Builder.CreateError(err)
	}
	err = s.protohelper.Generate(ctx)
	if err != nil {
		return s.Base.Builder.CreateError(err)
	}

	err = s.CreateEndpoints(ctx)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create endpoints")
	}

	return s.Base.Builder.CreateResponse(ctx, s.Settings)
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
