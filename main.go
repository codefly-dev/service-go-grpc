// Binary service-go-grpc is the gRPC specialization of the generic Go agent.
// It composes pkg/* types from github.com/codefly-dev/service-go and adds
// gRPC/REST/Connect endpoint handling, proto scaffolding, and hot reload.
package main

import (
	"context"
	"embed"
	"fmt"
	"path/filepath"
	"regexp"
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
	// Buf is rooted at the parent of this path, so the leaf must be a
	// directory Buf discovers by convention (a "proto"-named tree or one
	// carrying its own buf.yaml); an unconventional leaf name may generate
	// nothing.
	ProtocolSourceDir string `yaml:"protocol-source-dir"`
	// ProtocolOutputDirs names every Buf-owned output directory relative to
	// the service root (the directory holding proto/, code/, openapi/). Sync
	// replaces these trees exactly, including stale files left by renamed or
	// deleted protobuf declarations.
	ProtocolOutputDirs []string `yaml:"protocol-output-dirs"`

	// RuntimeAssets lists files or directories, relative to the service root,
	// that the service reads at runtime and that must ship in the final image
	// (e.g. "routing" for a service that loads REST routes from routing/rest at
	// startup). The final stage otherwise carries only the binary, so any asset
	// living outside the Go module works in dev — where the loader falls back to
	// a source-relative path — and vanishes in the container. Each path is
	// reproduced under /app, preserving its layout relative to the service root.
	RuntimeAssets []string `yaml:"runtime-assets"`

	// RuntimeImage overrides the codefly-built runtime image. Format:
	// "name:tag". :latest and untagged refs are rejected — pinning is
	// enforced. Leave empty to use codeflydev/go:<ver> (recommended).
	// Field named RuntimeImage (not DockerImage) to avoid colliding with
	// services.Base.DockerImage(req).
	RuntimeImage string `yaml:"docker-image"`

	// ServiceAccount binds the workload's pods to a named Kubernetes
	// ServiceAccount instead of the namespace default. Empty (the default)
	// leaves pods on the default SA. See ServiceAccountSpec.
	ServiceAccount *ServiceAccountSpec `yaml:"service-account,omitempty"`

	// Cors drives the generated REST listener's cross-origin policy. The zero
	// value (no `cors:` block) denies every cross-origin request — a
	// same-origin default. See CorsSpec.
	Cors CorsSpec `yaml:"cors,omitempty"`
}

// CorsSpec drives the CORS policy baked into the generated REST adapter
// (pkg/adapters/cors_gen.go). That file is agent-owned and must not be edited
// by hand, so cross-origin access is configured here instead. The zero value
// is a same-origin policy: every cross-origin request is refused.
type CorsSpec struct {
	// AllowedOrigins is the exact cross-origin allowlist. Empty (the default)
	// refuses every cross-origin request; same-origin traffic is unaffected.
	// A literal "*" is rejected by Validate — reach for AllowAll instead so the
	// wildcard is a deliberate, greppable choice rather than an allowlist typo.
	AllowedOrigins []string `yaml:"allowed-origins,omitempty"`

	// AllowedHeaders overrides the request headers a cross-origin caller may
	// send. Empty falls back to the rs/cors safe defaults (Accept,
	// Content-Type, X-Requested-With).
	AllowedHeaders []string `yaml:"allowed-headers,omitempty"`

	// AllowAll restores the permissive wildcard policy (any origin, any
	// header). It is the documented dev-mode escape hatch; anything reachable
	// beyond localhost should carry an explicit AllowedOrigins allowlist.
	AllowAll bool `yaml:"allow-all,omitempty"`
}

// Validate rejects a CORS block that would silently widen access. A literal
// "*" origin is refused so the wildcard cannot slip in as an allowlist entry —
// it must go through AllowAll — and AllowAll cannot be combined with an
// allowlist that it would silently ignore.
func (c CorsSpec) Validate() error {
	for _, origin := range c.AllowedOrigins {
		if origin == "*" {
			return fmt.Errorf(`cors: use allow-all instead of a "*" allowed-origins entry`)
		}
	}
	if c.AllowAll && len(c.AllowedOrigins) > 0 {
		return fmt.Errorf("cors: allow-all cannot be combined with allowed-origins")
	}
	return nil
}

// ServiceAccountSpec configures the Kubernetes ServiceAccount a service's
// pods run under. This is the passwordless-identity seam: annotations land
// on the rendered SA object (e.g. an Azure workload-identity client id) and
// labels stamp the pod template (e.g. azure.workload.identity/use: "true")
// so the identity webhook can inject the federated token.
type ServiceAccountSpec struct {
	Name        string            `yaml:"name"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
}

// dns1123Subdomain matches a Kubernetes ServiceAccount name (an RFC 1123 DNS
// subdomain).
var dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// Validate rejects a service-account block that would silently half-apply.
// Name is mandatory whenever the block is present: annotations render only on
// the SA object and serviceAccountName binds the pod, both keyed off Name — so
// annotations or labels without a Name would vanish, or worse stamp the
// workload-identity label onto a pod still running as the default SA (token
// minting then has no identity and DB connections fail with no deploy error).
// Requiring a valid Name keeps the SA object, serviceAccountName, and pod
// labels consistent and fails a bad name here rather than server-side.
func (s *ServiceAccountSpec) Validate() error {
	if s == nil {
		return nil
	}
	if s.Name == "" {
		return fmt.Errorf("service-account requires a name when set (got annotations/labels but no name)")
	}
	if len(s.Name) > 253 || !dns1123Subdomain.MatchString(s.Name) {
		return fmt.Errorf("service-account name %q must be a DNS-1123 subdomain", s.Name)
	}
	return nil
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
	for _, asset := range s.RuntimeAssets {
		if err := validateRuntimeAssetPath(asset); err != nil {
			return err
		}
	}
	if err := s.ServiceAccount.Validate(); err != nil {
		return err
	}
	if err := s.Cors.Validate(); err != nil {
		return err
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

// GoVersion is the exact Go patch release used for container builds.
const GoVersion = "1.27.0"

// AlpineVersion is the exact runtime Alpine patch release used for container builds.
const AlpineVersion = "3.23.5"

// Runtime Image
var runtimeImage = &configurations.DockerImage{Name: "codeflydev/go", Tag: "0.0.11"}

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
