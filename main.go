package main

import (
	"context"
	"embed"

	"google.golang.org/grpc/codes"

	"google.golang.org/grpc/status"

	"github.com/codefly-dev/core/templates"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/endpoints"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	basev1 "github.com/codefly-dev/core/generated/go/base/v1"
	agentv1 "github.com/codefly-dev/core/generated/go/services/agent/v1"
	"github.com/codefly-dev/core/shared"
)

// Agent version
var agent = shared.Must(configurations.LoadFromFs[configurations.Agent](shared.Embed(info)))

type Settings struct {
	Debug bool `yaml:"debug"` // Developer only

	Watch              bool `yaml:"watch"`
	WithDebugSymbols   bool `yaml:"with-debug-symbols"`
	CreateHttpEndpoint bool `yaml:"create-rest-endpoint"`
}

type Service struct {
	*services.Base

	// Endpoints
	GrpcEndpoint *basev1.Endpoint
	RestEndpoint *basev1.Endpoint

	// Settings
	*Settings
}

func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv1.AgentInformationRequest) (*agentv1.AgentInformation, error) {
	defer s.Wool.Catch()

	s.Wool.Debug("get agent information")

	readme, err := templates.ApplyTemplateFrom(shared.Embed(readme), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	s.DebugMe("readme success")

	return &agentv1.AgentInformation{
		Capabilities: []*agentv1.Capability{
			{Type: agentv1.Capability_FACTORY},
			{Type: agentv1.Capability_RUNTIME},
		},
		Languages: []*agentv1.Language{
			{Type: agentv1.Language_GO},
		},
		Protocols: []*agentv1.Protocol{
			{Type: agentv1.Protocol_HTTP},
			{Type: agentv1.Protocol_GRPC},
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
		case configurations.Grpc:
			s.GrpcEndpoint, err = endpoints.NewGrpcAPI(ctx, ep, s.Local("api.proto"))
			if err != nil {
				return s.Wrapf(err, "cannot create grpc api")
			}
			s.Endpoints = append(s.Endpoints, s.GrpcEndpoint)
			continue
		case configurations.Rest:
			s.RestEndpoint, err = endpoints.NewRestAPIFromOpenAPI(ctx, ep, s.Local("api.swagger.json"))
			if err != nil {
				return s.Wrapf(err, "cannot create openapi api")
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
