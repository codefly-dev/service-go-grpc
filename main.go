package main

import (
	"context"
	"embed"

	"github.com/codefly-dev/core/builders"

	"github.com/codefly-dev/core/configurations/standards"

	"google.golang.org/grpc/codes"

	"google.golang.org/grpc/status"

	"github.com/codefly-dev/core/templates"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	"github.com/codefly-dev/core/shared"
)

// Agent version
var agent = shared.Must(configurations.LoadFromFs[configurations.Agent](shared.Embed(infoFS)))

var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("service.codefly.yaml"),
	builders.NewDependency("src").WithPathSelect(shared.NewSelect("*.go")),
	builders.NewDependency("src/go.mod"),
)

type Settings struct {
	Debug bool `yaml:"debug"` // Developer only

	Watch bool `yaml:"watch"`

	WithDebugSymbols              bool `yaml:"with-debug-symbols"`
	WithRaceConditionDetectionRun bool `yaml:"with-race-condition-detection-run"`
	WithGRPCUnimplemented         bool `yaml:"with-grpc-unimplemented"`
	WithRestEndpoint              bool `yaml:"with-rest-endpoint"`
}

type Service struct {
	*services.Base

	// Endpoints
	GrpcEndpoint *basev0.Endpoint
	RestEndpoint *basev0.Endpoint

	// Settings
	*Settings
}

func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {
	defer s.Wool.Catch()

	readme, err := templates.ApplyTemplateFrom(ctx, shared.Embed(readmeFS), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &agentv0.AgentInformation{
		RuntimeRequirements: []*agentv0.Runtime{
			{Type: agentv0.Runtime_GO},
		},
		Capabilities: []*agentv0.Capability{
			{Type: agentv0.Capability_BUILDER},
			{Type: agentv0.Capability_RUNTIME},
		},
		Languages: []*agentv0.Language{
			{Type: agentv0.Language_GO},
		},
		Protocols: []*agentv0.Protocol{
			{Type: agentv0.Protocol_HTTP},
			{Type: agentv0.Protocol_GRPC},
		},
		ReadMe: readme,
	}, nil
}

func NewService() *Service {
	return &Service{
		Base:     services.NewServiceBase(context.Background(), agent),
		Settings: &Settings{},
	}
}

func (s *Service) LoadEndpoints(ctx context.Context, makePublic bool) error {
	defer s.Wool.Catch()
	s.Endpoints = []*basev0.Endpoint{}
	var err error
	for _, endpoint := range s.Configuration.Endpoints {
		endpoint.Application = s.Configuration.Application
		endpoint.Service = s.Configuration.Name
		switch endpoint.API {
		case standards.GRPC:
			s.GrpcEndpoint, err = configurations.NewGrpcAPI(ctx, endpoint, s.Local("proto/api.proto"))
			if err != nil {
				return s.Wool.Wrapf(err, "cannot create grpc api")
			}
			s.Endpoints = append(s.Endpoints, s.GrpcEndpoint)
			continue
		case standards.REST:
			// Useful when running locally
			if makePublic {
				endpoint.Visibility = configurations.VisibilityPublic
			}
			s.RestEndpoint, err = configurations.NewRestAPIFromOpenAPI(ctx, endpoint, s.Local("openapi/api.swagger.json"))
			if err != nil {
				return s.Wool.Wrapf(err, "cannot create openapi api")
			}
			s.Endpoints = append(s.Endpoints, s.RestEndpoint)
		}
	}
	return nil
}

func (s *Service) AddPublicRestEndpoint(ctx context.Context) {
	endpoint := configurations.CloneEndpoint(ctx, s.RestEndpoint)
	endpoint.Visibility = configurations.VisibilityPublic
	s.Endpoints = append(s.Endpoints, endpoint)
}

func main() {
	agents.Register(
		services.NewServiceAgent(agent.Of(configurations.ServiceAgent), NewService()),
		services.NewBuilderAgent(agent.Of(configurations.RuntimeServiceAgent), NewBuilder()),
		services.NewRuntimeAgent(agent.Of(configurations.BuilderServiceAgent), NewRuntime()))
}

//go:embed agent.codefly.yaml
var infoFS embed.FS

//go:embed templates/agent
var readmeFS embed.FS
