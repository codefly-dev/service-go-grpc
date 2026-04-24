package main

import (
	"testing"

	"gopkg.in/yaml.v3"

	goservice "github.com/codefly-dev/service-go/pkg/service"
)

// TestServiceInheritsFromGo verifies that go-grpc.Service embeds
// *goservice.Service and inherits the services.Base chain (Wool, Logger,
// Location, Identity). Breaking this breaks every gRPC method that
// references s.Wool or s.Base.
func TestServiceInheritsFromGo(t *testing.T) {
	svc := NewService()
	if svc == nil {
		t.Fatal("NewService returned nil")
	}
	if svc.Service == nil {
		t.Fatal("go-grpc.Service does not embed *goservice.Service")
	}
	if svc.Base == nil {
		t.Error("services.Base not promoted through embedding chain")
	}
	if svc.Settings == nil {
		t.Error("go-grpc Settings is nil")
	}
	if svc.Service.Settings == nil {
		t.Error("generic Settings (via embedded Service) is nil")
	}
}

// TestGoGrpcSettingsInheritsGo proves the YAML inline-embed: go-grpc YAML
// carries all GoAgentSettings (hot-reload, debug-symbols, race, cgo,
// workspace, source-dir) plus rest-endpoint / connect-endpoint.
func TestGoGrpcSettingsInheritsGo(t *testing.T) {
	src := []byte(`
hot-reload: true
debug-symbols: true
race-condition-detection-run: false
with-cgo: true
with-workspace: false
source-dir: "cmd/server"
rest-endpoint: true
connect-endpoint: false
`)
	var s Settings
	if err := yaml.Unmarshal(src, &s); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if !s.HotReload {
		t.Error("HotReload (from generic) not populated")
	}
	if !s.DebugSymbols {
		t.Error("DebugSymbols (from generic) not populated")
	}
	if !s.WithCGO {
		t.Error("WithCGO (from generic) not populated")
	}
	if s.SourceDir != "cmd/server" {
		t.Errorf("SourceDir (from generic): got %q", s.SourceDir)
	}
	if !s.RestEndpoint {
		t.Error("RestEndpoint (go-grpc) not populated")
	}
}

// TestSettingsPointerSharing proves that the generic Service.Settings
// points at the embedded half of go-grpc Settings so generic code paths
// see the shared fields.
func TestSettingsPointerSharing(t *testing.T) {
	svc := NewService()
	svc.Settings.HotReload = true
	gen, ok := interface{}(svc.Service.Settings).(*goservice.Settings)
	if !ok {
		t.Fatalf("generic Settings is not *goservice.Settings: %T", svc.Service.Settings)
	}
	if !gen.HotReload {
		t.Error("generic Settings did not reflect go-grpc mutation")
	}
}
