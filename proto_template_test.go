package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestProtoTemplatePinsGoPackage prevents a versioned protobuf namespace from
// silently changing the Go package imported by generated service adapters.
func TestProtoTemplatePinsGoPackage(t *testing.T) {
	protoTemplate, err := factoryFS.ReadFile("templates/factory/proto/api.proto.tmpl")
	if err != nil {
		t.Fatalf("read api proto template: %v", err)
	}
	if !strings.Contains(string(protoTemplate), `option go_package = "{{ .Service.Name.DNSCase }}/pkg/gen;gen";`) {
		t.Fatal("api proto template does not pin the generated Go package")
	}

	bufTemplate, err := factoryFS.ReadFile("templates/factory/proto/buf.gen.yaml.tmpl")
	if err != nil {
		t.Fatalf("read proto generation template: %v", err)
	}
	for _, setting := range []string{"enabled: false"} {
		if !strings.Contains(string(bufTemplate), setting) {
			t.Errorf("proto generation template does not contain %q", setting)
		}
	}
}

// TestProtoTemplateUsesBundledPlugins keeps fresh-service generation off the
// BSR remote-plugin path. The companion image already pins and ships these
// binaries; asking the BSR to execute them adds network failure and rate-limit
// risk without changing generated output.
func TestProtoTemplateUsesBundledPlugins(t *testing.T) {
	generationTemplate, err := factoryFS.ReadFile("templates/factory/proto/buf.gen.yaml.tmpl")
	if err != nil {
		t.Fatalf("read proto generation template: %v", err)
	}
	content := string(generationTemplate)
	for _, plugin := range []string{"path: protoc-gen-go", "path: protoc-gen-go-grpc", "path: protoc-gen-connect-go"} {
		if !strings.Contains(content, plugin) {
			t.Errorf("proto generation template does not use bundled plugin %q", plugin)
		}
	}
	if strings.Contains(content, "plugin: buf.build/") {
		t.Fatal("proto generation template still delegates a bundled plugin to the BSR")
	}

	moduleTemplate, err := factoryFS.ReadFile("templates/factory/proto/buf.yaml.tmpl")
	if err != nil {
		t.Fatalf("read Buf module template: %v", err)
	}
	if strings.Contains(string(moduleTemplate), "buf.build/bufbuild/protovalidate") {
		t.Fatal("Buf module template declares unused protovalidate sources")
	}
}

// TestFactoryDependencyLocksMatchBase makes the runnable base fixture the
// canonical dependency lock for newly generated services. Keeping a separate,
// silently stale template lock previously let conformance create services with
// patched dependencies in base/ but vulnerable versions in fresh projects.
func TestFactoryDependencyLocksMatchBase(t *testing.T) {
	baseMod, err := os.ReadFile("base/code/go.mod")
	if err != nil {
		t.Fatalf("read base go.mod: %v", err)
	}
	templateMod, err := factoryFS.ReadFile("templates/factory/code/go.mod.tmpl")
	if err != nil {
		t.Fatalf("read factory go.mod template: %v", err)
	}
	renderedMod := bytes.ReplaceAll(templateMod, []byte("{{ .Service.Name.DNSCase }}"), []byte("codefly-base"))
	if !bytes.Equal(baseMod, renderedMod) {
		t.Fatal("factory go.mod template drifted from base/code/go.mod")
	}

	baseSum, err := os.ReadFile("base/code/go.sum")
	if err != nil {
		t.Fatalf("read base go.sum: %v", err)
	}
	templateSum, err := factoryFS.ReadFile("templates/factory/code/go.sum.tmpl")
	if err != nil {
		t.Fatalf("read factory go.sum template: %v", err)
	}
	if !bytes.Equal(baseSum, templateSum) {
		t.Fatal("factory go.sum template drifted from base/code/go.sum")
	}

	agentMod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read agent go.mod: %v", err)
	}
	generatedCore := dependencyLine(baseMod, "github.com/codefly-dev/core")
	agentCore := dependencyLine(agentMod, "github.com/codefly-dev/core")
	if generatedCore == "" || generatedCore != agentCore {
		t.Fatalf("generated core dependency %q does not match agent dependency %q", generatedCore, agentCore)
	}
}

func TestBaseGeneratedServiceBuildsFromCleanModuleCache(t *testing.T) {
	t.Setenv("GOWORK", filepath.Join(t.TempDir(), "missing.go.work"))

	command := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "-modcacherw", "./...")
	command.Dir = "base/code"
	command.Env = append(os.Environ(),
		"GOMODCACHE="+t.TempDir(),
		"GOCACHE="+t.TempDir(),
		"GOWORK=off",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build generated service with clean caches: %v\n%s", err, output)
	}
}

func dependencyLine(goMod []byte, module string) string {
	for _, line := range strings.Split(string(goMod), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == module {
			return fields[0] + " " + fields[1]
		}
	}
	return ""
}

// TestServiceFlakeCarriesEveryBufPlugin keeps the Nix fallback equivalent to
// the pinned proto companion image. Sync must not become backend-dependent
// when Docker is temporarily unavailable.
func TestServiceFlakeCarriesEveryBufPlugin(t *testing.T) {
	template, err := factoryFS.ReadFile("templates/factory/flake.nix.tmpl")
	if err != nil {
		t.Fatalf("read service flake template: %v", err)
	}
	for _, tool := range []string{
		"pkgs.grpc-gateway",
		"pkgs.protoc-gen-connect-go",
	} {
		if !strings.Contains(string(template), tool) {
			t.Errorf("service flake template does not include %s", tool)
		}
	}
}

// TestGeneratedServiceHasPreStartCompositionSeam prevents the generated main
// package from forcing services to race dependency setup against live RPCs or
// edit generated code merely to install authentication interceptors.
func TestGeneratedServiceHasPreStartCompositionSeam(t *testing.T) {
	mainTemplate, err := factoryFS.ReadFile("templates/factory/code/main.go.tmpl")
	if err != nil {
		t.Fatalf("read main template: %v", err)
	}
	for _, want := range []string{"type Configure func", "WithConfigure", "configure(ctx, config)"} {
		if !strings.Contains(string(mainTemplate), want) {
			t.Errorf("main template does not contain %q", want)
		}
	}

	grpcTemplate, err := factoryFS.ReadFile("templates/factory/code/pkg/adapters/grpc_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read gRPC adapter template: %v", err)
	}
	for _, want := range []string{"GRPCServerOptions []grpc.ServerOption", "Service gen.{{ .Service.Name.Title }}ServiceServer", "grpc.NewServer(c.GRPCServerOptions...)", "if c.Service != nil"} {
		if !strings.Contains(string(grpcTemplate), want) {
			t.Errorf("gRPC adapter template does not contain %q", want)
		}
	}
	if !strings.Contains(string(grpcTemplate), "gen.Unimplemented{{ .Service.Name.Title }}ServiceServer") {
		t.Error("gRPC adapter does not remain source-compatible when protobuf methods are added")
	}

	connectTemplate, err := factoryFS.ReadFile("templates/factory/code/pkg/adapters/connect_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read Connect adapter template: %v", err)
	}
	if !strings.Contains(string(connectTemplate), "genconnect.Unimplemented{{ .Service.Name.Title }}ServiceHandler") {
		t.Error("disabled Connect adapter does not remain source-compatible when protobuf methods are added")
	}
}

func TestGeneratedServiceOmitsRESTImplementationWhenDisabled(t *testing.T) {
	mainTemplate, err := factoryFS.ReadFile("templates/factory/code/main.go.tmpl")
	if err != nil {
		t.Fatalf("read main template: %v", err)
	}
	for _, want := range []string{
		"if or .Settings.RestEndpoint .Settings.ConnectEndpoint",
		"if .Settings.RestEndpoint",
		"API(standards.REST).NetworkInstance()",
		"if .Settings.ConnectEndpoint",
		"API(standards.CONNECT).NetworkInstance()",
	} {
		if !strings.Contains(string(mainTemplate), want) {
			t.Errorf("generated main does not condition protocol discovery on settings: missing %q", want)
		}
	}

	serverTemplate, err := factoryFS.ReadFile("templates/factory/code/pkg/adapters/server_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read server adapter template: %v", err)
	}
	for _, want := range []string{
		"if .Settings.RestEndpoint",
		"rest    *RestServer",
		"rest:    rest",
		"if server.rest != nil",
		"server.rest.Shutdown(ctx)",
		"server.connect.Shutdown(ctx)",
	} {
		if !strings.Contains(string(serverTemplate), want) {
			t.Errorf("server adapter does not condition REST plumbing on settings: missing %q", want)
		}
	}

	restTemplate, err := factoryFS.ReadFile("templates/factory/code/pkg/adapters/rest_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read REST adapter template: %v", err)
	}
	if !strings.Contains(string(restTemplate), "{{- if .Settings.RestEndpoint }}") {
		t.Error("REST adapter implementation is emitted for gRPC-only services")
	}
	for _, templatePath := range []string{
		"templates/factory/code/pkg/adapters/rest_gen.go.tmpl",
		"templates/factory/code/pkg/adapters/connect_gen.go.tmpl",
	} {
		httpTemplate, err := factoryFS.ReadFile(templatePath)
		if err != nil {
			t.Fatalf("read HTTP adapter template: %v", err)
		}
		for _, want := range []string{"server *http.Server", "s.server.Shutdown(ctx)"} {
			if !strings.Contains(string(httpTemplate), want) {
				t.Errorf("%s does not contain %q", templatePath, want)
			}
		}
	}
}

// TestGeneratedServiceRegistersHealthChecks keeps the grpc.health.v1 service
// and the /healthz gateway route that the kustomize deployment probes target.
func TestGeneratedServiceRegistersHealthChecks(t *testing.T) {
	grpcTemplate, err := factoryFS.ReadFile("templates/factory/code/pkg/adapters/grpc_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read gRPC adapter template: %v", err)
	}
	for _, want := range []string{
		"health.NewServer()",
		"grpc_health_v1.RegisterHealthServer",
		`SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)`,
	} {
		if !strings.Contains(string(grpcTemplate), want) {
			t.Errorf("gRPC adapter template does not contain %q", want)
		}
	}

	serverTemplate, err := factoryFS.ReadFile("templates/factory/code/pkg/adapters/server_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read server adapter template: %v", err)
	}
	if !strings.Contains(string(serverTemplate), "server.grpc.health.Shutdown()") {
		t.Error("server adapter does not flip health to NOT_SERVING on shutdown")
	}

	restTemplate, err := factoryFS.ReadFile("templates/factory/code/pkg/adapters/rest_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read REST adapter template: %v", err)
	}
	for _, want := range []string{
		`HandlePath(http.MethodGet, "/healthz"`,
		"grpc_health_v1.NewHealthClient",
	} {
		if !strings.Contains(string(restTemplate), want) {
			t.Errorf("REST adapter template does not contain %q", want)
		}
	}
}

// TestGeneratedServiceGatesReflectionBehindIsLocal keeps gRPC server
// reflection — which enumerates every registered service and message for any
// unauthenticated caller — a local-only discovery aid, so deployed services do
// not disclose their API shape as needless attack surface.
func TestGeneratedServiceGatesReflectionBehindIsLocal(t *testing.T) {
	grpcTemplate, err := factoryFS.ReadFile("templates/factory/code/pkg/adapters/grpc_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read gRPC adapter template: %v", err)
	}
	content := string(grpcTemplate)

	gated := regexp.MustCompile(`if codefly\.IsLocal\(\) \{\s*reflection\.Register\(grpcServer\)\s*\}`)
	if !gated.MatchString(content) {
		t.Error("gRPC adapter template does not gate reflection registration behind codefly.IsLocal()")
	}
	if strings.Count(content, "reflection.Register(grpcServer)") != 1 {
		t.Error("gRPC adapter template registers reflection outside the codefly.IsLocal() gate")
	}
}

func TestGeneratedScaffoldSelectPreservesUserOwnedFiles(t *testing.T) {
	selectGenerated := generatedScaffoldSelect()
	for _, name := range []string{"code", "pkg", "adapters", "plugins", "main.go.tmpl", "grpc_gen.go.tmpl", "registry_gen.go.tmpl"} {
		if !selectGenerated.Keep(name) {
			t.Errorf("generated scaffold selection excludes %q", name)
		}
	}
	for _, name := range []string{"work.go.tmpl", "rpcs.go.tmpl", "go.mod.tmpl", "api.proto.tmpl", "README.md.tmpl", "plugins.yaml"} {
		if selectGenerated.Keep(name) {
			t.Errorf("generated scaffold selection would overwrite user-owned %q", name)
		}
	}
}

func TestGeneratedScaffoldTargetsRequireGeneratedRoot(t *testing.T) {
	root := t.TempDir()
	protoRoot := filepath.Join(root, "proto")
	writeTestFile(t, filepath.Join(protoRoot, "api.proto"), "syntax = \"proto3\"; service WidgetService {}\n")
	writeTestFile(t, filepath.Join(root, "code", "main.go"), "package main\n")
	targets, err := generatedScaffoldTargets(root, protoRoot, "WidgetService")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Fatalf("handwritten service claimed generated scaffolding: %v", targets)
	}

	writeTestFile(t, filepath.Join(root, "code", "main.go"), "// This code is generated by the agent\npackage main\n")
	targets, err = generatedScaffoldTargets(root, protoRoot, "WidgetService")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join("code", "main.go"),
		filepath.Join("code", "pkg", "adapters", "connect_gen.go"),
		filepath.Join("code", "pkg", "adapters", "cors_gen.go"),
		filepath.Join("code", "pkg", "adapters", "grpc_gen.go"),
		filepath.Join("code", "pkg", "adapters", "rest_gen.go"),
		filepath.Join("code", "pkg", "adapters", "server_gen.go"),
		filepath.Join("code", "plugins", "registry_gen.go"),
	}
	if !reflect.DeepEqual(targets, want) {
		t.Fatalf("generated scaffold targets = %v, want %v", targets, want)
	}

	writeTestFile(t, filepath.Join(protoRoot, "extra.proto"), "syntax = \"proto3\"; service OtherService {}\n")
	targets, err = generatedScaffoldTargets(root, protoRoot, "WidgetService")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Fatalf("multi-service protocol claimed single-service scaffolding: %v", targets)
	}
}
