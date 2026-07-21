package main

import (
	"path/filepath"
	"reflect"
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
	writeTestFile(t, filepath.Join(root, "code", "main.go"), "package main\n")
	targets, err := generatedScaffoldTargets(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Fatalf("handwritten service claimed generated scaffolding: %v", targets)
	}

	writeTestFile(t, filepath.Join(root, "code", "main.go"), "// This code is generated by the agent\npackage main\n")
	targets, err = generatedScaffoldTargets(root)
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
}
