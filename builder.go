package main

import (
	"context"
	"embed"
	"fmt"
	"github.com/codefly-dev/core/companions/proto"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"

	"github.com/codefly-dev/core/agents/communicate"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	"github.com/codefly-dev/core/agents/services"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
)

type Builder struct {
	*Service

	buf *proto.Buf

	cacheLocation string
}

func NewBuilder() *Builder {
	return &Builder{
		Service: NewService(),
	}
}

func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()

	ctx = s.Wool.Inject(ctx)

	err := s.Builder.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return nil, err
	}

	s.sourceLocation = s.Local("code")
	s.cacheLocation = s.Local(".cache")

	requirements.Localize(s.Location)

	if req.CreationMode != nil {
		s.Builder.CreationMode = req.CreationMode

		s.Builder.GettingStarted, err = templates.ApplyTemplateFrom(ctx, shared.Embed(factoryFS), "templates/factory/GETTING_STARTED.md", s.Information)
		if err != nil {
			return s.Builder.LoadError(err)
		}

		if req.CreationMode.Communicate {
			err = s.Communication.Register(ctx, communicate.New[builderv0.CreateRequest](s.createCommunicate()))
			if err != nil {
				return s.Builder.LoadError(err)
			}
		}
		return s.Builder.LoadResponse()
	}

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	s.GrpcEndpoint, err = resources.FindGRPCEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	if s.Settings.RestEndpoint {
		s.RestEndpoint, err = resources.FindRestEndpoint(ctx, s.Endpoints)
		if err != nil {
			return s.Builder.LoadError(err)
		}
	}

	return s.Builder.LoadResponse()
}

func (s *Builder) Init(ctx context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	defer s.Wool.Catch()

	s.Builder.LogInitRequest(req)

	ctx = s.Wool.Inject(ctx)

	s.DependencyEndpoints = req.DependenciesEndpoints

	return s.Builder.InitResponse()
}

func (s *Builder) Update(ctx context.Context, req *builderv0.UpdateRequest) (*builderv0.UpdateResponse, error) {
	defer s.Wool.Catch()

	ctx = s.Wool.Inject(ctx)

	return &builderv0.UpdateResponse{}, nil
}

func (s *Builder) Sync(ctx context.Context, req *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
	defer s.Wool.Catch()

	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("dependencies", wool.Field("dependencies", s.Service.Service.ServiceDependencies))
	for _, dep := range s.Service.Service.ServiceDependencies {
		ep, err := resources.FindGRPCEndpointFromService(ctx, dep, s.DependencyEndpoints)
		if err != nil {
			return s.Builder.SyncError(err)
		}
		if ep == nil {
			continue
		}
		err = proto.GenerateGRPC(ctx, languages.GO, s.Local("code/external/%s", dep.Unique()), dep.Unique(), ep)
		if err != nil {
			return s.Builder.SyncError(err)
		}
	}

	return s.Builder.SyncResponse()
}

type Env struct {
	Key   string
	Value string
}

type DockerTemplating struct {
	Components    []string
	Envs          []Env
	GoVersion     string
	AlpineVersion string
}

func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	defer s.Wool.Catch()

	ctx = s.Wool.Inject(ctx)

	dockerRequest, err := s.Builder.DockerBuildRequest(ctx, req)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "can only do docker build request")
	}

	image := s.DockerImage(dockerRequest)

	s.Wool.Debug("building docker image", wool.Field("image", image.FullName()))
	if !dockerhelpers.IsValidDockerImageName(image.Name) {
		return s.Builder.BuildError(fmt.Errorf("invalid docker image name: %s", image.Name))
	}

	docker := DockerTemplating{
		Components:    requirements.All(),
		GoVersion:     GoVersion,
		AlpineVersion: AlpineVersion,
	}

	err = shared.DeleteFile(ctx, s.Local("builder/Dockerfile"))
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
		Ignorefile:  "builder/dockerignore",
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
	s.Builder.WithDockerImages(image)
	return s.Builder.BuildResponse()
}

type Parameters struct {
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	ctx = s.Wool.Inject(ctx)

	s.Builder.LogDeployRequest(req, s.Wool.Debug)

	s.EnvironmentVariables.SetRunning()

	var k *builderv0.KubernetesDeployment
	var err error
	if k, err = s.Builder.KubernetesDeploymentRequest(ctx, req); err != nil {
		return s.Builder.DeployError(err)
	}

	err = s.EnvironmentVariables.AddEndpoints(ctx,
		resources.LocalizeNetworkMapping(req.NetworkMappings, "localhost"),
		resources.NewContainerNetworkAccess())
	if err != nil {
		return s.Builder.DeployError(err)
	}

	err = s.EnvironmentVariables.AddEndpoints(ctx, req.DependenciesNetworkMappings,
		resources.NewContainerNetworkAccess())
	if err != nil {
		return s.Builder.DeployError(err)
	}

	err = s.EnvironmentVariables.AddConfigurations(ctx, req.Configuration)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	err = s.EnvironmentVariables.AddConfigurations(ctx, req.DependenciesConfigurations...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	cm, err := services.EnvsAsConfigMapData(s.EnvironmentVariables.Configurations()...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	secrets, err := services.EnvsAsSecretData(s.EnvironmentVariables.Secrets()...)
	if err != nil {
		return s.Builder.DeployError(err)
	}

	params := services.DeploymentParameters{
		ConfigMap:  cm,
		SecretMap:  secrets,
		Parameters: Parameters{},
	}

	err = s.Builder.KustomizeDeploy(ctx, req.Environment, k, deploymentFS, params)
	if err != nil {
		return s.Builder.DeployError(err)
	}
	return s.Builder.DeployResponse()
}

func (s *Builder) CreateEndpoints(ctx context.Context) error {
	grpc, err := resources.LoadGrpcAPI(ctx, shared.Pointer(s.Local(standards.ProtoPath)))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot load grpc api")
	}
	endpoint := s.Base.BaseEndpoint(standards.GRPC)
	s.GrpcEndpoint, err = resources.NewAPI(ctx, endpoint, resources.ToGrpcAPI(grpc))

	s.Endpoints = append(s.Endpoints, s.GrpcEndpoint)

	if s.Settings.RestEndpoint {
		rest, err := resources.LoadRestAPI(ctx, shared.Pointer(s.Local(standards.OpenAPIPath)))
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create openapi api")
		}
		endpoint = s.Base.BaseEndpoint(standards.REST)
		s.RestEndpoint, err = resources.NewAPI(ctx, endpoint, resources.ToRestAPI(rest))
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create openapi api")
		}
		s.Endpoints = append(s.Endpoints, s.RestEndpoint)
	}
	return nil
}

func (s *Builder) Options() []*agentv0.Question {
	return []*agentv0.Question{
		communicate.NewConfirm(&agentv0.Message{Name: HotReload, Message: "Code hot-reload (Recommended)?", Description: "codefly can restart your service when code changes are detected 🔎"}, true),
		communicate.NewConfirm(&agentv0.Message{Name: DebugSymbols, Message: "Start with debug symbols?", Description: "Build the go binary with debug symbol to use stack debugging"}, false),
		communicate.NewConfirm(&agentv0.Message{Name: RaceConditionDetectionRun, Message: "Start with race condition detection?", Description: "Build the go binary with race condition detection"}, false),
		communicate.NewConfirm(&agentv0.Message{Name: RestEndpoint, Message: "Automatic REST generation (Recommended)?", Description: "codefly can generate a REST server that stays magically 🪄 synced to your gRPC definition -- the easiest way to do REST"}, true),
	}
}

func (s *Builder) createCommunicate() *communicate.Sequence {
	return communicate.NewSequence(s.Options()...)

}

type CreateConfiguration struct {
	*services.Information
	Envs []string
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()

	ctx = s.Wool.Inject(ctx)

	if s.Builder.CreationMode.Communicate {
		s.Wool.Debug("using communicate mode")

		session, err := s.Communication.Done(ctx, communicate.Channel[builderv0.CreateRequest]())
		if err != nil {
			return s.Builder.CreateErrorf(err, "cannot find a communication")
		}

		s.Settings.HotReload, err = session.Confirm(HotReload)
		if err != nil {
			return s.Builder.CreateError(err)
		}

		s.Settings.DebugSymbols, err = session.Confirm(DebugSymbols)
		if err != nil {
			return s.Builder.CreateError(err)
		}

		s.Settings.RaceConditionDetectionRun, err = session.Confirm(RaceConditionDetectionRun)
		if err != nil {
			return s.Builder.CreateError(err)
		}

		s.Settings.RestEndpoint, err = session.Confirm(RestEndpoint)
		if err != nil {
			return s.Builder.CreateError(err)
		}
	} else {
		options := s.Options()
		var err error
		s.Settings.HotReload, err = communicate.GetDefaultConfirm(options, HotReload)
		if err != nil {
			return s.Builder.CreateError(err)
		}
		s.Settings.DebugSymbols, err = communicate.GetDefaultConfirm(options, DebugSymbols)
		if err != nil {
			return s.Builder.CreateError(err)
		}

		s.Settings.RaceConditionDetectionRun, err = communicate.GetDefaultConfirm(options, RaceConditionDetectionRun)
		if err != nil {
			return s.Builder.CreateError(err)
		}

		s.Settings.RestEndpoint, err = communicate.GetDefaultConfirm(options, RestEndpoint)
		if err != nil {
			return s.Builder.CreateError(err)
		}

	}

	create := CreateConfiguration{
		Information: s.Information,
		Envs:        []string{},
	}
	ignore := shared.NewIgnore("go.work*", "service.generation.codefly.yaml")

	override := shared.OverrideException(shared.NewIgnore("*.proto")) // Don't override proto

	err := s.Templates(ctx, create, services.WithFactory(factoryFS).WithPathSelect(ignore).WithOverride(override))
	if err != nil {
		return s.Builder.CreateError(err)
	}

	err = s.CreateEndpoints(ctx)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create endpoints")
	}

	return s.Builder.CreateResponse(ctx, s.Settings)
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
