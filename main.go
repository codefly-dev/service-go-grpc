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
var agent = shared.Must(configurations.LoadFromFs[configurations.Agent](shared.Embed(info)))

var requirements = &builders.Dependency{Components: []string{"pkg", "main.go", "proto"}, Ignore: shared.NewSelect("*.go", "*.proto")}

type Settings struct {
	Debug bool `yaml:"debug"` // Developer only

	Watch              bool `yaml:"watch"`
	WithDebugSymbols   bool `yaml:"with-debug-symbols"`
	CreateHttpEndpoint bool `yaml:"create-rest-endpoint"`
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

	readme, err := templates.ApplyTemplateFrom(shared.Embed(readme), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &agentv0.AgentInformation{
		RuntimeRequirements: []*agentv0.Runtime{
			{Type: agentv0.Runtime_GO},
		},
		Capabilities: []*agentv0.Capability{
			{Type: agentv0.Capability_FACTORY},
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

func (s *Service) LoadEndpoints(ctx context.Context) error {
	defer s.Wool.Catch()
	var err error
	for _, ep := range s.Configuration.Endpoints {
		switch ep.API {
		case standards.GRPC:
			s.GrpcEndpoint, err = configurations.NewGrpcAPI(ctx, ep, s.Local("proto/api.proto"))
			if err != nil {
				return s.Wool.Wrapf(err, "cannot create grpc api")
			}
			s.Endpoints = append(s.Endpoints, s.GrpcEndpoint)
			continue
		case standards.REST:
			s.RestEndpoint, err = configurations.NewRestAPIFromOpenAPI(ctx, ep, s.Local("proto/swagger/api.swagger.json"))
			if err != nil {
				return s.Wool.Wrapf(err, "cannot create openapi api")
			}
			s.Endpoints = append(s.Endpoints, s.RestEndpoint)
			continue
		}
	}
	return nil
}

func main() {
	agents.Register(
		services.NewServiceAgent(agent.Of(configurations.ServiceAgent), NewService()),
		services.NewFactoryAgent(agent.Of(configurations.RuntimeServiceAgent), NewFactory()),
		services.NewRuntimeAgent(agent.Of(configurations.FactoryServiceAgent), NewRuntime()))
}

//go:embed agent.codefly.yaml
var info embed.FS

//go:embed templates/agent
var readme embed.FS
