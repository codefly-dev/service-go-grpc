// Binary service-go-grpc is the gRPC specialization of the generic Go agent.
// It composes pkg/* types from github.com/codefly-dev/service-go and adds
// gRPC/REST/Connect endpoint handling, proto scaffolding, and hot reload.
package main

import (
	"context"
	"embed"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	configurations "github.com/codefly-dev/core/resources"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"

	gocode "github.com/codefly-dev/service-go/pkg/code"
	goruntime "github.com/codefly-dev/service-go/pkg/runtime"
	goservice "github.com/codefly-dev/service-go/pkg/service"
	gotooling "github.com/codefly-dev/service-go/pkg/tooling"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Agent version.
var agent = shared.Must(configurations.LoadFromFs[configurations.Agent](shared.Embed(infoFS)))

var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("service.codefly.yaml"),
	builders.NewDependency("code").WithPathSelect(shared.NewSelect("*.go")),
)

// Settings extends the generic Go Settings with go-grpc-specific toggles.
// yaml:",inline" keeps the YAML shape flat: go-grpc services see all
// generic fields (hot-reload, debug-symbols, …) plus rest-endpoint /
// connect-endpoint at the same level.
type Settings struct {
	goservice.Settings `yaml:",inline"`

	RestEndpoint    bool `yaml:"rest-endpoint"`
	ConnectEndpoint bool `yaml:"connect-endpoint"`

	// RuntimeImage overrides the codefly-built runtime image. Format:
	// "name:tag". :latest and untagged refs are rejected — pinning is
	// enforced. Leave empty to use codeflydev/go:<ver> (recommended).
	// Field named RuntimeImage (not DockerImage) to avoid colliding with
	// services.Base.DockerImage(req).
	RuntimeImage string `yaml:"docker-image"`
}

// Setting names re-exported for local use (templates, Builder options).
const (
	HotReload                 = golanghelpers.SettingHotReload
	DebugSymbols              = golanghelpers.SettingDebugSymbols
	RaceConditionDetectionRun = golanghelpers.SettingRaceConditionDetectionRun
	RestEndpointSetting       = "rest-endpoint"
	ConnectEndpointSetting    = "connect-endpoint"
)

// Service is the go-grpc specialization. It embeds *goservice.Service to
// inherit Base + generic Settings, and adds the three protocol endpoints.
type Service struct {
	*goservice.Service

	// Specialization settings (shadows generic Settings via the Settings
	// field — callers reaching s.Settings get this richer struct).
	Settings *Settings

	GrpcEndpoint    *basev0.Endpoint
	RestEndpoint    *basev0.Endpoint
	ConnectEndpoint *basev0.Endpoint
}

// GetAgentInformation overrides generic to add HTTP/GRPC protocols and
// goGrpcTechniques. Specializations pattern across the ecosystem.
func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {
	defer s.Wool.Catch()

	info := s.Information
	if info == nil {
		info = &services.Information{}
	}
	readme, err := templates.ApplyTemplateFrom(ctx, shared.Embed(readmeFS), "templates/agent/README.md", info)
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
		ReadMe:     readme,
		Techniques: goGrpcTechniques(),
	}, nil
}

func NewService() *Service {
	generic := goservice.New(agent)
	settings := &Settings{}
	generic.Settings = &settings.Settings
	return &Service{
		Service:  generic,
		Settings: settings,
	}
}

// GoVersion is the Go toolchain version used for container builds.
const GoVersion = "1.26"

// AlpineVersion is the base Alpine version for container builds.
const AlpineVersion = "3.21"

// Runtime Image
var runtimeImage = &configurations.DockerImage{Name: "codeflydev/go", Tag: "0.0.10"}

func main() {
	svc := NewService()

	// Code and Tooling inherit wholesale from the generic Go layer —
	// go-grpc has no language-level analysis behavior to add beyond what
	// generic already provides (corecode.GoCodeServer + goimports/gofmt).
	code := gocode.New(svc.Service)
	genericRuntime := goruntime.New(svc.Service)

	agents.Serve(agents.PluginRegistration{
		Agent:   svc,
		Runtime: NewRuntime(svc),
		Builder: NewBuilder(svc),
		Code:    code,
		Tooling: gotooling.New(code, genericRuntime),
	})
}

//go:embed agent.codefly.yaml
var infoFS embed.FS

//go:embed templates/agent
var readmeFS embed.FS
