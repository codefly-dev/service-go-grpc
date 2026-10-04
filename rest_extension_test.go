package main

import (
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
)

// renderScaffold renders a factory adapter template through core's real
// template engine, the way the generation pipeline does, and gofmts the result
// the way the existing gRPC and CORS drift locks do. A plain string replacement
// of the two placeholders is no longer enough: the Configuration fields these
// tests cover are emitted under a settings condition, which only the engine
// evaluates.
//
// The case fields are set directly rather than through shared.ToCase because
// base's module path ("codefly-base") is not derived from its service name
// ("web"); the two placeholders the adapter templates use are what matter.
func renderScaffold(t *testing.T, templatePath string, settings *Settings) string {
	t.Helper()
	raw, err := factoryFS.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("read %s: %v", templatePath, err)
	}
	create := CreateConfiguration{
		Information: &services.Information{
			Service: &resources.ServiceWithCase{Name: shared.Case{DNSCase: "codefly-base", Title: "Web"}},
		},
		Settings: settings,
	}
	rendered, err := templates.ApplyTemplate(string(raw), create)
	if err != nil {
		t.Fatalf("apply %s: %v", templatePath, err)
	}
	formatted, err := format.Source([]byte(rendered))
	if err != nil {
		t.Fatalf("format rendered %s: %v\n%s", templatePath, err, rendered)
	}
	return string(formatted)
}

// TestFactoryRestAdapterMatchesBase binds the generated REST listener in base/
// to its factory template. The REST adapter was the scaffold file carrying the
// CORS wrapper, the health route and now both extension seams, with no drift
// lock at all: base/ could gain a seam and freshly created services not get it
// (or the reverse) and CI would stay green as long as both still compiled.
func TestFactoryRestAdapterMatchesBase(t *testing.T) {
	baseRest, err := os.ReadFile(filepath.Join("base", "code", "pkg", "adapters", "rest_gen.go"))
	if err != nil {
		t.Fatalf("read base REST adapter: %v", err)
	}
	rendered := renderScaffold(t, "templates/factory/code/pkg/adapters/rest_gen.go.tmpl", &Settings{RestEndpoint: true})
	if rendered != string(baseRest) {
		t.Fatalf("factory REST adapter template drifted from base/code/pkg/adapters/rest_gen.go\n--- rendered ---\n%s", rendered)
	}
}

// TestFactoryAdapterReadmeMatchesBase keeps the contract shipped into every
// service equal to the one checked into base/. The README template carries no
// placeholders, so the two are byte-identical or one of them is stale — and a
// stale copy of this document is worse than none: it is what a service author
// reads before deciding whether to hand-edit a generated file.
func TestFactoryAdapterReadmeMatchesBase(t *testing.T) {
	baseReadme, err := os.ReadFile(filepath.Join("base", "code", "pkg", "adapters", "README.md"))
	if err != nil {
		t.Fatalf("read base adapters README: %v", err)
	}
	template, err := factoryFS.ReadFile("templates/factory/code/pkg/adapters/README.md.tmpl")
	if err != nil {
		t.Fatalf("read factory adapters README template: %v", err)
	}
	if string(template) != string(baseReadme) {
		t.Fatal("factory adapters README template drifted from base/code/pkg/adapters/README.md")
	}
}

// TestGeneratedRestListenerExposesExtensionSeams pins the wiring that makes
// Configuration.ServeMuxOptions and Configuration.Routes reachable at all.
// Each assertion is a way the seam has already been lost in a hand-edited copy
// of this file:
//
//   - the option list must be a named function, so a service's tests build the
//     same mux the server runs rather than a copy that drifts from it;
//   - Run must pass the configured options into that function, not ignore them;
//   - the routes must wrap the gateway *inside* the CORS handler, so a mounted
//     handler answers under the same policy as the gateway.
func TestGeneratedRestListenerExposesExtensionSeams(t *testing.T) {
	raw, err := factoryFS.ReadFile("templates/factory/code/pkg/adapters/rest_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read REST adapter template: %v", err)
	}
	for _, want := range []string{
		"func gatewayMuxOptions(extra ...runtime.ServeMuxOption) []runtime.ServeMuxOption",
		"func gatewayDialOptions(extra ...grpc.DialOption) []grpc.DialOption",
		"return append(options, extra...)",
		"gwMux := runtime.NewServeMux(gatewayMuxOptions(s.config.ServeMuxOptions...)...)",
		"opts := gatewayDialOptions(s.config.GatewayDialOptions...)",
		"type Route struct",
		"func WithRoutes(routes []Route, next http.Handler) http.Handler",
		"func WithMiddleware(middleware []func(http.Handler) http.Handler, next http.Handler) http.Handler",
		// The chain, and the two positions that are contract rather than taste:
		// the middleware inside the CORS policy, the logging outermost.
		"handler := WithMiddleware(s.config.Middleware, WithRoutes(s.config.Routes, gwMux))",
		"s.server.Handler = logRequestOutcome(c.Handler(handler))",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("REST adapter template does not contain %q", want)
		}
	}

	grpcRaw, err := factoryFS.ReadFile("templates/factory/code/pkg/adapters/grpc_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read gRPC adapter template: %v", err)
	}
	for _, want := range []string{
		"ServeMuxOptions []runtime.ServeMuxOption",
		"Routes []Route",
		"Middleware []func(http.Handler) http.Handler",
		"GatewayDialOptions []grpc.DialOption",
	} {
		if !strings.Contains(string(grpcRaw), want) {
			t.Errorf("Configuration does not declare %q", want)
		}
	}
}

// TestGeneratedRestListenerLogsNoRequestPayload keeps the generated listener off
// the handler it used to ship: one that read every request body into memory so
// it could log the payload on any non-200 response. That wrote the caller's data
// — a search query, a document — into a log store with its own retention and
// audience, on exactly the paths where something had already gone wrong, and the
// buffering kept any handler on this listener from being reached before EOF.
//
// base/code's TestRESTListenerDispatchesBeforeTheRequestBodyCompletes holds the
// behaviour; this holds the shape, because a body could be read back into the
// log by a change that still dispatches promptly.
func TestGeneratedRestListenerLogsNoRequestPayload(t *testing.T) {
	raw, err := factoryFS.ReadFile("templates/factory/code/pkg/adapters/rest_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read REST adapter template: %v", err)
	}
	base, err := os.ReadFile(filepath.Join("base", "code", "pkg", "adapters", "rest_gen.go"))
	if err != nil {
		t.Fatalf("read base REST adapter: %v", err)
	}
	for name, content := range map[string]string{"template": string(raw), "base": string(base)} {
		for _, unwanted := range []string{"io.ReadAll(r.Body)", "logRequestBody", "request body %+v"} {
			if strings.Contains(content, unwanted) {
				t.Errorf("the generated REST listener (%s) still carries %q", name, unwanted)
			}
		}
		if !strings.Contains(content, "func logRequestOutcome(h http.Handler) http.Handler") {
			t.Errorf("the generated REST listener (%s) records no request outcome at all", name)
		}
	}
}

// TestGeneratedRestExtensionSeamsFollowTheRestSetting keeps the two
// Configuration fields and the Route type they need rendered by one condition.
// Split apart they break in both directions: a field without its type does not
// compile for a gRPC-only service, and a field a service can set on a listener
// the scaffold never runs is a silently dropped setting, which this agent
// refuses by contract.
func TestGeneratedRestExtensionSeamsFollowTheRestSetting(t *testing.T) {
	withREST := renderScaffold(t, "templates/factory/code/pkg/adapters/grpc_gen.go.tmpl", &Settings{RestEndpoint: true})
	restAdapter := renderScaffold(t, "templates/factory/code/pkg/adapters/rest_gen.go.tmpl", &Settings{RestEndpoint: true})
	for _, want := range []string{
		"ServeMuxOptions []runtime.ServeMuxOption",
		"Routes []Route",
		"Middleware []func(http.Handler) http.Handler",
		"GatewayDialOptions []grpc.DialOption",
	} {
		if !strings.Contains(withREST, want) {
			t.Errorf("a REST service's Configuration is missing %q", want)
		}
	}
	for _, want := range []string{"type Route struct", "func WithMiddleware("} {
		if !strings.Contains(restAdapter, want) {
			t.Errorf("a REST service's adapter is missing %q, which its Configuration refers to", want)
		}
	}

	withoutREST := renderScaffold(t, "templates/factory/code/pkg/adapters/grpc_gen.go.tmpl", &Settings{})
	restDisabled := renderScaffold(t, "templates/factory/code/pkg/adapters/rest_gen.go.tmpl", &Settings{})
	for _, unwanted := range []string{
		"ServeMuxOptions",
		"Routes []Route",
		"Middleware []func",
		"GatewayDialOptions",
		"grpc-gateway/v2/runtime",
		`"net/http"`,
	} {
		if strings.Contains(withoutREST, unwanted) {
			t.Errorf("a gRPC-only service's Configuration still carries %q", unwanted)
		}
	}
	for _, unwanted := range []string{"type Route struct", "func WithRoutes", "func WithMiddleware"} {
		if strings.Contains(restDisabled, unwanted) {
			t.Errorf("a gRPC-only service still gets %q:\n%s", unwanted, restDisabled)
		}
	}
}

// TestBaseGeneratedServiceServesRESTExtensionSeams runs the generated base
// service's own REST extension suite, which boots the listeners and calls them
// over real HTTP (base/code/pkg/adapters/rest_extension_test.go): a supplied
// forward-response option answering with an ETag and a 304, a supplied option
// replacing a generated one, a prefix-mounted handler served ahead of the
// gateway while /version and /healthz still reach it, the service's dial options
// reaching both the gateway hop and the health client, middleware wrapping the
// chain outermost-first, and a chunked request reaching its handler before the
// body completes.
//
// base/code is a separate module, so the agent's `go test ./...` does not reach
// it; without this runner the regression #154 reports would have coverage that
// CI never executes. It needs the network on a cold module cache, like
// TestBaseGeneratedServiceBuildsFromCleanModuleCache beside it.
func TestBaseGeneratedServiceServesRESTExtensionSeams(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "test", "-mod=readonly", "-count=1", "-run", "^TestREST", "./pkg/adapters/")
	command.Dir = filepath.Join("base", "code")
	command.Env = append(os.Environ(), "GOWORK=off")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("generated service REST extension suite: %v\n%s", err, output)
	}
}
