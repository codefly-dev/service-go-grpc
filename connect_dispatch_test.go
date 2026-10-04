package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
	"golang.org/x/tools/imports"
)

// baseCreateConfiguration is the template context that renders the checked-in
// base fixture: the base module path with the service title its proto declares,
// and the settings base is generated with (REST and Connect both served).
//
// shared.Case is populated field by field rather than through shared.ToCase
// because base's module path ("codefly-base") is not derived from its service
// name ("web"); the two placeholders the adapter templates use are what matters
// here.
func baseCreateConfiguration(settings *Settings) CreateConfiguration {
	return CreateConfiguration{
		Information: &services.Information{
			Service: &resources.ServiceWithCase{Name: shared.Case{DNSCase: "codefly-base", Title: "Web"}},
		},
		Settings: settings,
	}
}

// renderFactoryGo renders a factory Go template through the engine the
// generation pipeline uses, then through the goimports pass sync applies to
// every staged file (builder.go, formatStagedGo). The result is therefore the
// bytes a real sync writes into a service, which is what base/ must equal.
func renderFactoryGo(t *testing.T, templatePath string, settings *Settings) []byte {
	t.Helper()
	raw, err := factoryFS.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("read %s: %v", templatePath, err)
	}
	rendered, err := templates.ApplyTemplate(string(raw), baseCreateConfiguration(settings))
	if err != nil {
		t.Fatalf("apply %s: %v", templatePath, err)
	}
	formatted, err := imports.Process(templatePath, []byte(rendered), &imports.Options{Comments: true})
	if err != nil {
		t.Fatalf("format rendered %s: %v\n%s", templatePath, err, rendered)
	}
	return formatted
}

// TestFactoryConnectAdapterMatchesBase binds the generated Connect listener in
// base/ to its factory template, the way TestFactoryGrpcAdapterMatchesBase does
// for the gRPC one. Without it, the Connect adapter was the one scaffold file
// with no drift lock: base/ could be fixed and fresh services left broken, or
// the reverse, and CI would stay green as long as both still compiled.
func TestFactoryConnectAdapterMatchesBase(t *testing.T) {
	baseConnect, err := os.ReadFile(filepath.Join("base", "code", "pkg", "adapters", "connect_gen.go"))
	if err != nil {
		t.Fatalf("read base Connect adapter: %v", err)
	}
	rendered := renderFactoryGo(t, "templates/factory/code/pkg/adapters/connect_gen.go.tmpl", &Settings{RestEndpoint: true, ConnectEndpoint: true})
	if string(rendered) != string(baseConnect) {
		t.Fatalf("factory Connect adapter template drifted from base/code/pkg/adapters/connect_gen.go\n--- rendered ---\n%s", rendered)
	}
}

// TestFactoryServerAdapterMatchesBase does the same for the startup file that
// wires the three listeners together. It is where the Connect listener receives
// the gRPC server it transcodes to, so a template-only change there would
// otherwise leave base/ compiling against the old constructor signature.
func TestFactoryServerAdapterMatchesBase(t *testing.T) {
	baseServer, err := os.ReadFile(filepath.Join("base", "code", "pkg", "adapters", "server_gen.go"))
	if err != nil {
		t.Fatalf("read base server adapter: %v", err)
	}
	rendered := renderFactoryGo(t, "templates/factory/code/pkg/adapters/server_gen.go.tmpl", &Settings{RestEndpoint: true, ConnectEndpoint: true})
	if string(rendered) != string(baseServer) {
		t.Fatalf("factory server adapter template drifted from base/code/pkg/adapters/server_gen.go\n--- rendered ---\n%s", rendered)
	}
}

// TestGeneratedConnectListenerDispatchesThroughTheGRPCServer pins the contract
// that fixes #155: the Connect listener transcodes to the gRPC server, so the
// implementation in Configuration.Service and the policy in
// Configuration.GRPCServerOptions serve Connect callers too.
//
// The listener used to install a Connect handler of its own, which implemented
// Version and inherited every other method as unimplemented. Reintroducing any
// Connect-side handler is what this test refuses: a handler there answers
// without the interceptors the gRPC server holds, so an authority or operations
// guard a service installs would silently not apply to Connect traffic — and
// Runnable operations are invoked over Connect with JSON.
func TestGeneratedConnectListenerDispatchesThroughTheGRPCServer(t *testing.T) {
	connectTemplate, err := factoryFS.ReadFile("templates/factory/code/pkg/adapters/connect_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read Connect adapter template: %v", err)
	}
	content := string(connectTemplate)
	for _, want := range []string{
		"func NewConnectServer(c *Configuration, grpcServer *grpc.Server) (*ConnectServer, error)",
		"vanguardgrpc.NewTranscoder(grpcServer",
		"grpcServer.GetServiceInfo()",
		// gRPC refuses an encoding it has no decompressor for, and refuses it as
		// Unimplemented: without this the listener answers a request-compressing
		// client with the very error this fix removes. Decompressing inside gRPC
		// also keeps the configured receive bound applied to the inflated
		// message rather than outside it.
		`_ "google.golang.org/grpc/encoding/gzip"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("Connect adapter template does not contain %q", want)
		}
	}
	for _, unwanted := range []string{
		"genconnect.Unimplemented",
		"ServiceHandler(",
		"connectHandler",
	} {
		if strings.Contains(content, unwanted) {
			t.Errorf("Connect adapter template installs its own Connect handler (%q): substantive RPCs and transport policy would diverge from the gRPC listener", unwanted)
		}
	}

	serverTemplate, err := factoryFS.ReadFile("templates/factory/code/pkg/adapters/server_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read server adapter template: %v", err)
	}
	if !strings.Contains(string(serverTemplate), "NewConnectServer(config, grpc.gRPC)") {
		t.Error("server adapter does not hand the gRPC server to the Connect listener")
	}
	// Plugins register their services on the gRPC server, and the transcoder
	// routes what is registered when it is built, so the Connect listener has to
	// be constructed after that loop.
	plugins := strings.Index(string(serverTemplate), "p.RegisterGRPC(grpc.gRPC)")
	connect := strings.Index(string(serverTemplate), "NewConnectServer(config, grpc.gRPC)")
	if plugins < 0 || connect < 0 || plugins > connect {
		t.Error("the Connect listener is built before plugins register, so plugin services are unreachable over Connect")
	}
}

// TestGeneratedServerAdapterRendersWithoutREST keeps a gRPC-only service
// renderable. The startup file's REST plumbing is behind a template
// conditional, and renderFactoryGo fails on output Go cannot parse, so this
// covers the rendering no base/ fixture exercises: the Connect listener still
// receives the gRPC server when the REST blocks are absent.
func TestGeneratedServerAdapterRendersWithoutREST(t *testing.T) {
	rendered := string(renderFactoryGo(t, "templates/factory/code/pkg/adapters/server_gen.go.tmpl", &Settings{ConnectEndpoint: true}))
	if strings.Contains(rendered, "RestServer") {
		t.Errorf("REST plumbing rendered for a service with REST disabled:\n%s", rendered)
	}
	if !strings.Contains(rendered, "NewConnectServer(config, grpc.gRPC)") {
		t.Errorf("the Connect listener does not receive the gRPC server:\n%s", rendered)
	}
}

// TestBaseGeneratedServiceServesConfiguredRPCsOverConnect runs the generated
// base service's own adapter suite, which boots the three listeners and calls
// the configured service over real Connect, gRPC-Web and gRPC HTTP
// (base/code/pkg/adapters/connect_gen_test.go).
//
// base/code is a separate module, so the agent's `go test ./...` does not reach
// it; without this runner the regression that #155 reports would have coverage
// that CI never executes. It needs the network on a cold module cache, like
// TestBaseGeneratedServiceBuildsFromCleanModuleCache beside it.
func TestBaseGeneratedServiceServesConfiguredRPCsOverConnect(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "test", "-mod=readonly", "-count=1", "./pkg/adapters/")
	command.Dir = filepath.Join("base", "code")
	command.Env = append(os.Environ(), "GOWORK=off")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("generated service adapter suite: %v\n%s", err, output)
	}
}
