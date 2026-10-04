package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
)

// A versioned service's Go import is declared by its own proto, not by the
// scaffold's historical default or an imported option descriptor.
func TestSyncScaffoldUsesTheServicesDeclaredGoPackage(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "options.proto"), "syntax = \"proto3\"; option go_package = \"example.com/options;options\";")
	writeTestFile(t, filepath.Join(root, "api", "v1", "api.proto"), "syntax = \"proto3\"; option go_package = \"example.com/service/gen/api/v1;apiv1\"; service WidgetService {}")
	goPackage, err := declaredServiceGoPackage(root, "WidgetService")
	if err != nil {
		t.Fatal(err)
	}
	if goPackage != "example.com/service/gen/api/v1;apiv1" {
		t.Fatalf("wrong service package: %q", goPackage)
	}
	create := CreateConfiguration{
		Information: &services.Information{Service: &resources.ServiceWithCase{Name: shared.Case{DNSCase: "widget", Title: "Widget"}}},
		Settings:    &Settings{RestEndpoint: true, ConnectEndpoint: true}, ProtoGoPackage: goPackage,
	}
	if create.GeneratedConnectImport() != "example.com/service/gen/api/v1/apiv1connect" {
		t.Fatal("Connect import ignored the declared package name")
	}
	for _, adapter := range []string{"grpc", "rest"} {
		raw, err := factoryFS.ReadFile("templates/factory/code/pkg/adapters/" + adapter + "_gen.go.tmpl")
		if err != nil {
			t.Fatal(err)
		}
		rendered, err := templates.ApplyTemplate(string(raw), create)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rendered, `gen "example.com/service/gen/api/v1"`) {
			t.Fatalf("%s scaffold ignored go_package", adapter)
		}
	}
}
