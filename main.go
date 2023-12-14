package main

import (
	"context"
	"embed"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/endpoints"
	basev1 "github.com/codefly-dev/core/generated/v1/go/proto/base"
	agentv1 "github.com/codefly-dev/core/generated/v1/go/proto/services/agent"
	"github.com/codefly-dev/core/shared"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
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
	return &agentv1.AgentInformation{
		Capabilities: []*agentv1.Capability{
			{Type: agentv1.Capability_FACTORY},
			{Type: agentv1.Capability_RUNTIME},
		},
	}, nil
}

func NewService() *Service {
	return &Service{
		Base:     services.NewServiceBase(shared.NewContext(), agent.Of(configurations.ServiceAgent)),
		Settings: &Settings{},
	}
}

func (s *Service) LoadEndpoints() error {
	defer s.AgentLogger.Catch()
	var err error
	for _, ep := range s.Configuration.Endpoints {
		switch ep.API {
		case configurations.Grpc:
			s.GrpcEndpoint, err = endpoints.NewGrpcAPI(ep, s.Local("api.proto"))
			if err != nil {
				return s.Wrapf(err, "cannot create grpc api")
			}
			s.Endpoints = append(s.Endpoints, s.GrpcEndpoint)
			continue
		case configurations.Rest:
			s.RestEndpoint, err = endpoints.NewRestAPIFromOpenAPI(s.Context(), ep, s.Local("api.swagger.json"))
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
