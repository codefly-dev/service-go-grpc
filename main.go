// Binary service-go-grpc is the gRPC specialization of the generic Go agent.
// It composes pkg/* types from github.com/codefly-dev/service-go and adds
// gRPC/REST/Connect endpoint handling, proto scaffolding, and hot reload.
package main

import (
	"context"
	"embed"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/languages"
	configurations "github.com/codefly-dev/core/resources"
	runnersbase "github.com/codefly-dev/core/runners/base"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
	"github.com/codefly-dev/core/toolbox/lang"

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
	// ProtocolSourceDir locates the Buf source directory relative to the
	// service root. The default is "proto"; nested Go modules may opt into a
	// path such as "code/proto" without moving their public protocol tree.
	ProtocolSourceDir string `yaml:"protocol-source-dir"`
	// ProtocolOutputDirs names every Buf-owned output directory relative to
	// the service root (the directory holding proto/, code/, openapi/). Sync
	// replaces these trees exactly, including stale files left by renamed or
	// deleted protobuf declarations.
	ProtocolOutputDirs []string `yaml:"protocol-output-dirs"`

	// RuntimeImage overrides the codefly-built runtime image. Format:
	// "name:tag". :latest and untagged refs are rejected — pinning is
	// enforced. Leave empty to use codeflydev/go:<ver> (recommended).
	// Field named RuntimeImage (not DockerImage) to avoid colliding with
	// services.Base.DockerImage(req).
	RuntimeImage string `yaml:"docker-image"`
}

func (s *Settings) Validate() error {
	if err := s.GoAgentSettings.Validate(); err != nil {
		return err
	}
	sourceDir := s.protocolSourceDir()
	if !filepath.IsLocal(sourceDir) || sourceDir == "." || strings.ContainsAny(sourceDir, "\x00\\") {
		return fmt.Errorf("protocol source directory %q must stay below the service root", sourceDir)
	}
	for _, dir := range s.protocolOutputDirs() {
		if !filepath.IsLocal(dir) || dir == "." || strings.ContainsAny(dir, "\x00\\") {
			return fmt.Errorf("protocol output directory %q must stay below the service root", dir)
		}
	}
	return nil
}

func (s *Settings) protocolSourceDir() string {
	if s.ProtocolSourceDir == "" {
		return "proto"
	}
	return s.ProtocolSourceDir
}

func (s *Settings) protocolOutputDirs() []string {
	if len(s.ProtocolOutputDirs) == 0 {
		return []string{"code/pkg/gen", "openapi"}
	}
	return append([]string(nil), s.ProtocolOutputDirs...)
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

	validation := goservice.ValidationCapabilities()
	validation.Sync.Supported = true

	return services.Advertisement{
		Backends: runnersbase.BackendSupport{
			Local:  func() bool { return languages.HasGoRuntime(nil) },
			Nix:    true,
			Docker: true,
		},
		Toolchains: []agentv0.Toolchain_Type{agentv0.Toolchain_GO},
		Languages:  []agentv0.Language_Type{agentv0.Language_GO},
		Protocols:  []agentv0.Protocol_Type{agentv0.Protocol_HTTP, agentv0.Protocol_GRPC},
		ReadMe:     readme,
		Techniques: goGrpcTechniques(),
		Validation: validation,
	}.Build(), nil
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
	tooling := gotooling.New(code, genericRuntime)

	agents.Serve(agents.PluginRegistration{
		Agent:   svc,
		Runtime: NewRuntime(svc),
		Builder: NewBuilder(svc),
		Code:    code,
		Tooling: tooling,
		Toolbox: lang.NewToolboxFromTooling(agent.Name, agent.Version, tooling),
	})
}

//go:embed agent.codefly.yaml
var infoFS embed.FS

//go:embed templates/agent
var readmeFS embed.FS
