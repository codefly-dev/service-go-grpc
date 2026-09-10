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
	// same-origin default. See CorsSpec. No omitempty: a struct value is never
	// "empty" to the YAML encoder, so the tag would be a silent no-op.
	Cors CorsSpec `yaml:"cors"`

	// Health declares which health contract the service actually serves, so
	// the Kubernetes manifests probe that contract instead of guessing. Absent
	// means transport-only: a service customized before this declaration
	// existed keeps the probes it has always had. See HealthSpec.
	Health *HealthSpec `yaml:"health,omitempty"`
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

// Validate rejects a CORS block that would silently widen access or silently
// drop configuration. A literal "*" origin is refused so the wildcard cannot
// slip in as an allowlist entry — it must go through AllowAll. Any field the
// generated adapter would ignore is a hard error rather than a no-op, because a
// setting that vanishes is worse than one that fails loudly.
func (c CorsSpec) Validate() error {
	for _, origin := range c.AllowedOrigins {
		if origin == "*" {
			return fmt.Errorf(`cors: use allow-all instead of a "*" allowed-origins entry`)
		}
	}
	if c.AllowAll {
		// allow-all is the "everything" policy; the generated adapter emits a
		// wildcard and ignores any allowlist, so a supplied one would silently
		// vanish. Reject the combination instead.
		if len(c.AllowedOrigins) > 0 || len(c.AllowedHeaders) > 0 {
			return fmt.Errorf("cors: allow-all cannot be combined with allowed-origins or allowed-headers")
		}
		return nil
	}
	// A header allowlist only takes effect alongside an origin allowlist. With
	// no origins the policy denies every cross-origin request and the configured
	// headers would never reach the generated adapter — fail rather than drop.
	if len(c.AllowedHeaders) > 0 && len(c.AllowedOrigins) == 0 {
		return fmt.Errorf("cors: allowed-headers requires allowed-origins")
	}
	return nil
}

// DeniesCrossOrigin reports whether this spec resolves to the default
// same-origin policy — no explicit allowlist and no allow-all opt-in — under
// which the generated adapter refuses every cross-origin request. It gates the
// Sync-time warning that flags this behavior change to services regenerating an
// older, wildcard-open adapter.
func (c CorsSpec) DeniesCrossOrigin() bool {
	return !c.AllowAll && len(c.AllowedOrigins) == 0
}

// HealthMode names the health contract a service declares it serves. The
// deployment templates render the Kubernetes probe that speaks that contract:
// a transport probe proves only that a listener accepts connections, while the
// semantic modes prove the application answers.
type HealthMode string

const (
	// HealthModeTransport opens a TCP connection to the gRPC listener. It
	// proves the socket is bound and nothing more, so an application wedged
	// behind an accepted connection still reads as healthy. It is the mode a
	// service keeps when it declares no health block.
	HealthModeTransport HealthMode = "tcp"

	// HealthModeGrpc calls the standard gRPC health service
	// (grpc.health.v1.Health) that the generated server registers, and is
	// ready only on SERVING. The kubelet speaks this natively from Kubernetes
	// 1.24 (beta, on by default) and 1.27 (GA); on anything older the probe is
	// rejected, so a cluster that predates it must declare tcp explicitly
	// rather than have a semantic predicate silently downgraded. The built-in
	// probe also dials in plaintext: a gRPC listener behind TLS or per-call
	// authentication cannot be probed this way.
	HealthModeGrpc HealthMode = "grpc"

	// HealthModeHTTP issues a GET against a route the service declares it
	// serves. The kubelet accepts any 2xx or 3xx response; a stricter
	// predicate (an exact status, a response body) is not expressible with a
	// built-in probe.
	HealthModeHTTP HealthMode = "http"
)

// HealthSpec is the declared health capability the deployment manifests render
// probes from. Mode is mandatory whenever the block is present — a health block
// with no mode is a half-written declaration, not a request for the default.
//
// Only readiness and startup follow the declared mode. Liveness stays
// transport-only in every mode: a semantic liveness probe reports "a
// dependency is unavailable" as "this process is wedged", and the kubelet
// answers that by restarting pods that were working fine.
type HealthSpec struct {
	Mode HealthMode `yaml:"mode"`

	// GrpcService names the entry to check in the gRPC health service. Empty
	// (the default) checks the server-wide entry, which the generated server
	// registers alongside the per-service one; set it to a fully qualified
	// proto service name to gate readiness on that service alone. Validate
	// constrains it to that shape, and the template quotes it: the value is
	// interpolated straight into a rendered manifest.
	GrpcService string `yaml:"grpc-service,omitempty"`

	// Path is the HTTP route to probe. It has no default: the generic
	// deployment contract guarantees a listener, never a route, so a route
	// this agent invented would 404 on every service that does not happen to
	// serve it. Validate constrains it to URL path characters, and the
	// template quotes it, for the same reason as GrpcService.
	Path string `yaml:"path,omitempty"`

	// Port selects which HTTP listener serves Path: "http" (the grpc-gateway
	// REST facade) or "connect". Defaults to "http".
	Port string `yaml:"port,omitempty"`
}

// healthPortEndpoints maps an HTTP health port to the setting that binds it.
var healthPortEndpoints = map[string]string{"http": RestEndpointSetting, "connect": ConnectEndpointSetting}

// grpcServiceName matches a fully qualified protobuf service name, which is the
// only thing the gRPC health service can be keyed by.
var grpcServiceName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

// healthRoutePath matches an absolute URL path built from RFC 3986 path
// characters. It excludes whitespace, control characters, and "#" — all of
// which change what the rendered manifest means rather than what it probes.
var healthRoutePath = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*$`)

// validateHealth rejects a health declaration the rendered probes could not
// honor. It reads the service's own listener settings, so a route on a port the
// process never opens fails here rather than as a pod that restart-loops
// forever against a closed port. Fields belonging to another mode are refused
// rather than ignored: a silently dropped predicate is how a service ends up
// believing it checks more than it does.
func (s *Settings) validateHealth() error {
	h := s.Health
	if h == nil {
		return nil
	}
	switch h.Mode {
	case HealthModeTransport:
		if h.GrpcService != "" || h.Path != "" || h.Port != "" {
			return fmt.Errorf("health: mode %q takes no grpc-service, path, or port", h.Mode)
		}
	case HealthModeGrpc:
		if h.Path != "" || h.Port != "" {
			return fmt.Errorf("health: mode %q takes no path or port — it probes the gRPC listener", h.Mode)
		}
		if h.GrpcService != "" && !grpcServiceName.MatchString(h.GrpcService) {
			return fmt.Errorf("health: grpc-service %q must be a fully qualified protobuf service name", h.GrpcService)
		}
	case HealthModeHTTP:
		if h.GrpcService != "" {
			return fmt.Errorf("health: mode %q takes no grpc-service", h.Mode)
		}
		if !strings.HasPrefix(h.Path, "/") {
			return fmt.Errorf("health: mode %q requires an absolute path the service serves (got %q)", h.Mode, h.Path)
		}
		if !healthRoutePath.MatchString(h.Path) {
			return fmt.Errorf("health: path %q must be a URL path — no whitespace, control characters, or %q", h.Path, "#")
		}
		port := h.Port
		if port == "" {
			port = "http"
		}
		setting, known := healthPortEndpoints[port]
		if !known {
			return fmt.Errorf("health: port %q is not a listener this service can bind (want http or connect)", port)
		}
		if (port == "http" && !s.RestEndpoint) || (port == "connect" && !s.ConnectEndpoint) {
			return fmt.Errorf("health: port %q requires %s: true", port, setting)
		}
	case "":
		return fmt.Errorf("health: mode is required (%s, %s, or %s)", HealthModeTransport, HealthModeGrpc, HealthModeHTTP)
	default:
		return fmt.Errorf("health: unsupported mode %q (want %s, %s, or %s)", h.Mode, HealthModeTransport, HealthModeGrpc, HealthModeHTTP)
	}
	return nil
}

// Handler reports the mode the templates render a probe for. It fails on an
// unset mode rather than falling back: the zero value of the rendered spec
// would otherwise mean "transport" here while meaning "incomplete declaration"
// to validateHealth, so a parameter set built without Normalized would quietly
// downgrade a service's semantic probes to a TCP connect. Returning an error
// makes template execution fail instead.
func (h HealthSpec) Handler() (HealthMode, error) {
	switch h.Mode {
	case HealthModeTransport, HealthModeGrpc, HealthModeHTTP:
		return h.Mode, nil
	}
	return "", fmt.Errorf("health: deployment parameters carry no health mode; Normalized must resolve one before rendering")
}

// Normalized resolves the declaration the deployment templates render from. A
// missing block resolves to transport-only, so a service written before this
// declaration existed renders exactly the probes it renders today.
func (h *HealthSpec) Normalized() HealthSpec {
	if h == nil {
		return HealthSpec{Mode: HealthModeTransport}
	}
	resolved := *h
	if resolved.Mode == HealthModeHTTP && resolved.Port == "" {
		resolved.Port = "http"
	}
	return resolved
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
	if err := s.validateHealth(); err != nil {
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
