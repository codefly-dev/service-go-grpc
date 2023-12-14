package main

import (
	"context"
	"embed"
	"fmt"
	"github.com/codefly-dev/core/agents/communicate"
	"github.com/codefly-dev/core/agents/endpoints"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	golanghelpers "github.com/codefly-dev/core/agents/helpers/go"
	protohelpers "github.com/codefly-dev/core/agents/helpers/proto"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	agentsv1 "github.com/codefly-dev/core/generated/v1/go/proto/agents"
	servicev1 "github.com/codefly-dev/core/generated/v1/go/proto/services"
	factoryv1 "github.com/codefly-dev/core/generated/v1/go/proto/services/factory"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
	"os"
)

type Factory struct {
	*Service

	protohelper *protohelpers.Proto
	gohelper    golanghelpers.Go
}

func NewFactory() *Factory {
	return &Factory{
		Service: NewService(),
	}
}
func (p *Factory) Init(ctx context.Context, req *servicev1.InitRequest) (*factoryv1.InitResponse, error) {
	defer p.AgentLogger.Catch()

	err := p.Base.Init(req, p.Settings)
	if err != nil {
		return nil, err
	}
	p.DebugMe("init success")

	err = p.LoadEndpoints()
	if err != nil {
		return p.FactoryInitResponseError(err)
	}
	p.DebugMe("load endpoint success")

	// communication on CreateResponse
	err = p.Communication.Register(ctx, communicate.New[factoryv1.CreateRequest](createCommunicate()))
	if err != nil {
		return p.FactoryInitResponseError(err)
	}

	if err != nil {
		return p.FactoryInitResponseError(err)
	}
	p.DebugMe("communicate success")

	readme, err := templates.ApplyTemplateFrom(shared.Embed(factory), "templates/factory/README.md", p.Information)
	if err != nil {
		return p.FactoryInitResponseError(err)
	}
	p.DebugMe("readme success")

	return p.FactoryInitResponse(p.Endpoints, readme)
}

const Watch = "with-hot-reload"
const WithDebugSymbols = "with-debug-symbols"
const WithRest = "create-rest-endpoint"

func createCommunicate() *communicate.Sequence {
	return communicate.NewSequence(
		communicate.NewConfirm(&agentsv1.Message{Name: Watch, Message: "Code hot-reload (Recommended)?", Description: "codefly can restart your service when code changes are detected 🔎"}, true),
		communicate.NewConfirm(&agentsv1.Message{Name: WithRest, Message: "Automatic REST generation (Recommended)?", Description: "codefly can generate a REST server that stays magically 🪄 synced to your gRPC definition -- the easiest way to do REST"}, true),
		communicate.NewConfirm(&agentsv1.Message{Name: WithDebugSymbols, Message: "Run with debug symbols?", Description: "Build the go binary with debug symbol to use stack debugging"}, true),
	)
}

type CreateConfiguration struct {
	*services.Information
	Image *configurations.DockerImage
	Envs  []string
}

func (p *Factory) Create(ctx context.Context, req *factoryv1.CreateRequest) (*factoryv1.CreateResponse, error) {
	defer p.AgentLogger.Catch()

	session, err := p.Communication.Done(ctx, communicate.Channel[factoryv1.CreateRequest]())
	if err != nil {
		return p.CreateResponseError(err)
	}

	p.Settings.Watch, err = session.Confirm(Watch)
	if err != nil {
		return p.CreateResponseError(err)
	}
	p.Settings.CreateHttpEndpoint, err = session.Confirm(WithRest)
	if err != nil {
		return p.CreateResponseError(err)
	}

	p.Settings.WithDebugSymbols, err = session.Confirm(WithDebugSymbols)
	if err != nil {
		return p.CreateResponseError(err)
	}

	create := CreateConfiguration{
		Information: p.Information,
		Image:       p.DockerImage(),
		Envs:        []string{},
	}
	ignores := []string{"go.work", "service.generation.codefly.yaml"}
	err = p.Templates(p.Context(), create,
		services.WithFactory(factory, ignores...),
		services.WithBuilder(builder))
	if err != nil {
		return nil, err
	}

	out, err := shared.GenerateTree(p.Location, " ")
	if err != nil {
		return nil, err
	}
	p.AgentLogger.Info("tree: %s", out)
	p.ServiceLogger.Info("We generated this code for you:\n%s", out)

	err = p.CreateEndpoints()
	if err != nil {
		return nil, p.Wrapf(err, "cannot create endpoints")
	}

	p.protohelper, err = protohelpers.NewProto(p.Location)
	if err != nil {
		return nil, p.Wrapf(err, "cannot create proto helper")
	}

	err = p.protohelper.Generate(p.Context())
	if err != nil {
		return nil, fmt.Errorf("factory>create: go gohelper: cannot run buf generate: %v", err)
	}

	p.gohelper = golanghelpers.Go{Dir: p.Location}

	err = p.gohelper.ModTidy(p.Context())
	if err != nil {
		return nil, fmt.Errorf("factory>create: go gohelper: cannot run mod tidy: %v", err)
	}

}

func (p *Factory) Update(ctx context.Context, req *factoryv1.UpdateRequest) (*factoryv1.UpdateResponse, error) {
	defer p.AgentLogger.Catch()

	p.ServiceLogger.Info("Updating")

	err := p.Base.Templates(nil, services.WithBuilder(builder))
	if err != nil {
		return nil, p.Wrapf(err, "cannot copy and apply template")
	}

	helper := golanghelpers.Go{Dir: p.Location}
	err = helper.Update(p.Context())
	if err != nil {
		return nil, fmt.Errorf("factory>update: go helper: cannot run update: %v", err)
	}
	return &factoryv1.UpdateResponse{}, nil
}

func (p *Factory) Sync(ctx context.Context, req *factoryv1.SyncRequest) (*factoryv1.SyncResponse, error) {
	defer p.AgentLogger.Catch()

	p.AgentLogger.TODO("Some caching please!")

	p.AgentLogger.Debugf("running sync: %v", p.Location)

	// Clean-up the generated code
	p.AgentLogger.TODO("get location of generated code from buf")

	err := os.RemoveAll(p.Local("adapters/servicev1"))
	if err != nil {
		return nil, p.Wrapf(err, "cannot remove adapters")
	}
	// Re-generate
	p.AgentLogger.TODO("change buf to use openapi or not depending on things...")

	err = p.protohelper.Generate(p.Context())
	if err != nil {
		return nil, p.Wrapf(err, "cannot generate proto")
	}

	err = p.gohelper.ModTidy(p.Context())
	if err != nil {
		return nil, p.Wrapf(err, "cannot tidy go.mod")
	}

	return &factoryv1.SyncResponse{}, nil
}

type Env struct {
	Key   string
	Value string
}

type DockerTemplating struct {
	Envs []Env
}

func (p *Factory) Build(ctx context.Context, req *factoryv1.BuildRequest) (*factoryv1.BuildResponse, error) {
	p.AgentLogger.Debugf("building docker image")
	docker := DockerTemplating{}

	e, err := endpoints.FromProtoEndpoint(p.GrpcEndpoint)
	if err != nil {
		return nil, p.Wrapf(err, "cannot convert grpc endpoint")
	}
	gRPC := configurations.AsEndpointEnvironmentVariableKey(p.Configuration.Application, p.Configuration.Name, e)
	docker.Envs = append(docker.Envs, Env{Key: gRPC, Value: "localhost:9090"})
	if p.RestEndpoint != nil {
		e, err = endpoints.FromProtoEndpoint(p.RestEndpoint)
		if err != nil {
			return nil, p.Wrapf(err, "cannot convert grpc endpoint")
		}
		rest := configurations.AsEndpointEnvironmentVariableKey(p.Configuration.Application, p.Configuration.Name, e)
		docker.Envs = append(docker.Envs, Env{Key: rest, Value: "localhost:8080"})
	}

	err = os.Remove(p.Local("codefly/builder/Dockerfile"))
	if err != nil {
		return nil, p.Wrapf(err, "cannot remove dockerfile")
	}
	err = p.Templates(p.Context(), docker, services.WithBuilder(builder))
	if err != nil {
		return nil, p.Wrapf(err, "cannot copy and apply template")
	}
	image := p.DockerImage()
	builder, err := dockerhelpers.NewBuilder(dockerhelpers.BuilderConfiguration{
		Root:       p.Location,
		Dockerfile: "codefly/builder/Dockerfile",
		Image:      image.Name,
		Tag:        image.Tag,
	})
	if err != nil {
		return nil, p.Wrapf(err, "cannot create builder")
	}
	builder.WithLogger(p.AgentLogger)
	_, err = builder.Build()
	if err != nil {
		return nil, p.Wrapf(err, "cannot build image")
	}
	return &factoryv1.BuildResponse{}, nil
}

type Deployment struct {
	Replicas int
}

type DeploymentParameter struct {
	Image *configurations.DockerImage
	*services.Information
	Deployment
}

func (p *Factory) Deploy(ctx context.Context, req *factoryv1.DeploymentRequest) (*factoryv1.DeploymentResponse, error) {
	defer p.AgentLogger.Catch()
	deploy := DeploymentParameter{Image: p.DockerImage(), Information: p.Information, Deployment: Deployment{Replicas: 1}}
	err := p.Templates(p.Context(), deploy) //services.WithDeploymentFor(deployment, "kustomize/base", templates.WithOverrideAll()),
	//services.WithDeploymentFor(deployment, "kustomize/overlays/environment",
	//	services.WithDestination("kustomize/overlays/%s", req.Environment.Name), templates.WithOverrideAll()),

	if err != nil {
		return nil, err
	}
	return &factoryv1.DeploymentResponse{}, nil
}

func (p *Factory) CreateEndpoints() error {
	grpc, err := endpoints.NewGrpcAPI(&configurations.Endpoint{Name: "grpc"}, p.Local("api.proto"))
	if err != nil {
		return p.Wrapf(err, "cannot create grpc api")
	}
	p.Endpoints = append(p.Endpoints, grpc)

	if p.Settings.CreateHttpEndpoint {
		rest, err := endpoints.NewRestAPIFromOpenAPI(p.Context(), &configurations.Endpoint{Name: "rest", Visibility: "private"}, p.Local("api.swagger.json"))
		if err != nil {
			return p.Wrapf(err, "cannot create openapi api")
		}
		p.Endpoints = append(p.Endpoints, rest)
	}
	return nil
}

//go:embed templates/factory
var factory embed.FS

//go:embed templates/builder
var builder embed.FS

//go:embed templates/deployment
var deployment embed.FS
