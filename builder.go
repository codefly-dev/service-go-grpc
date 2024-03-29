package main

import (
	"context"
	"embed"
	"fmt"
	"github.com/codefly-dev/core/configurations/standards"
	"github.com/codefly-dev/core/generators"

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

	gohelper *golanghelpers.Go

	protohelper *generators.Proto
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

	s.sourceLocation = s.Local("src")

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

	s.NetworkMappings = req.ProposedNetworkMappings

	return s.Builder.InitResponse()
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

	image := s.DockerImage(req.BuildContext)

	if !dockerhelpers.IsValidDockerImageName(image.Name) {
		return s.Builder.BuildError(fmt.Errorf("invalid docker image name: %s", image.Name))
	}

	docker := DockerTemplating{
		Components: requirements.All(),
	}

	endpoint := configurations.EndpointFromProto(s.grpcEndpoint)
	gRPC := configurations.EndpointEnvironmentVariableKey(endpoint.Information())
	docker.Envs = append(docker.Envs, Env{Key: gRPC, Value: standards.LocalhostAddress(standards.GRPC)})

	if s.restEndpoint != nil {
		endpoint = configurations.EndpointFromProto(s.restEndpoint)
		rest := configurations.EndpointEnvironmentVariableKey(endpoint.Information())
		docker.Envs = append(docker.Envs, Env{Key: rest, Value: standards.LocalhostAddress(standards.REST)})
	}

	err := shared.DeleteFile(ctx, s.Local("builder/Dockerfile"))
	if err != nil {
		return s.Builder.BuildError(err)
	}

	err = s.Templates(ctx, docker, services.WithBuilder(builderFS))
	if err != nil {
		return s.Builder.BuildError(err)
	}

	builder, err := dockerhelpers.NewBuilder(dockerhelpers.BuilderConfiguration{
		Root:        s.Location,
		Dockerfile:  "builder/Dockerfile",
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

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	envs, err := configurations.ExtractEndpointEnvironmentVariables(ctx, req.NetworkMappings)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	cfMap, err := services.EnvsAsConfigMapData(envs...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	params := services.DeploymentParameters{
		ConfigMap: cfMap,
	}

	err = s.Builder.GenericServiceDeploy(ctx, req, deploymentFS, params)
	if err != nil {
		return s.Builder.DeployError(err)
	}
	return s.Builder.DeployResponse()
}

func (s *Builder) CreateEndpoints(ctx context.Context) error {
	grpc, err := configurations.NewGrpcAPI(ctx, s.Configuration.BaseEndpoint(standards.GRPC), s.Local("proto/api.proto"))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create grpc api")
	}
	s.grpcEndpoint = grpc
	s.Endpoints = append(s.Endpoints, grpc)

	if s.Settings.WithRestEndpoint {
		rest, err := configurations.NewRestAPIFromOpenAPI(ctx, s.Configuration.BaseEndpoint(standards.REST), s.Local("openapi/api.swagger.json"))
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create openapi api")
		}
		s.restEndpoint = rest
		s.Endpoints = append(s.Endpoints, rest)
	}
	return nil
}

const Watch = "with-hot-reload"
const WithDebugSymbols = "with-debug-symbols"
const WithRaceConditionDetectionRun = "with-race-condition-detection-run"
const WithRestEndpoint = "with-rest-endpoint"

func createCommunicate() *communicate.Sequence {
	return communicate.NewSequence(
		communicate.NewConfirm(&agentv0.Message{Name: Watch, Message: "Code hot-reload (Recommended)?", Description: "codefly can restart your service when code changes are detected 🔎"}, true),
		communicate.NewConfirm(&agentv0.Message{Name: WithDebugSymbols, Message: "Start with debug symbols?", Description: "Build the go binary with debug symbol to use stack debugging"}, false),
		communicate.NewConfirm(&agentv0.Message{Name: WithRaceConditionDetectionRun, Message: "Start with race condition detection?", Description: "Build the go binary with race condition detection"}, false),
		communicate.NewConfirm(&agentv0.Message{Name: WithRestEndpoint, Message: "Automatic REST generation (Recommended)?", Description: "codefly can generate a REST server that stays magically 🪄 synced to your gRPC definition -- the easiest way to do REST"}, true),
	)
}

type CreateConfiguration struct {
	*services.Information
	Envs []string
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()

	ctx = s.WoolAgent.Inject(ctx)

	session, err := s.Communication.Done(ctx, communicate.Channel[builderv0.CreateRequest]())
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

	s.Settings.WithRestEndpoint, err = session.Confirm(WithRestEndpoint)
	if err != nil {
		return s.Builder.CreateError(err)
	}

	create := CreateConfiguration{
		Information: s.Information,
		Envs:        []string{},
	}
	ignore := shared.NewIgnore("go.work*", "service.generation.codefly.yaml")
	override := shared.OverrideException(shared.NewIgnore("*.proto")) // Don't override proto

	err = s.Templates(ctx, create, services.WithFactory(factoryFS).WithPathSelect(ignore).WithOverride(override))
	if err != nil {
		return s.Base.Builder.CreateError(err)
	}

	s.gohelper = &golanghelpers.Go{Dir: s.sourceLocation}
	_, err = s.gohelper.ModTidy(ctx)
	if err != nil {
		return s.Base.Builder.CreateError(err)
	}

	s.protohelper, err = generators.NewProto(ctx, s.Location)
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
