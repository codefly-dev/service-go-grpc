package main

import (
	"context"
	"encoding/base64"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	agenttesting "github.com/codefly-dev/core/agents/testing"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8syaml "sigs.k8s.io/yaml"
)

// undeclaredHealth is what a service that declares no health block resolves to.
// Rendering requires a resolved contract — the zero value is refused — so tests
// that are not about health still go through the same resolution production
// does.
func undeclaredHealth() HealthSpec { return (*HealthSpec)(nil).Normalized() }

func TestDeploymentTemplates(t *testing.T) {
	agenttesting.AssertKustomizeTemplates(t, deploymentFS, DeploymentParameters{Health: undeclaredHealth()})
}

// TestDeploymentTemplatesHaveNoOrphans catches source-level orphans: any file
// under templates/deployment (template or not) that sits outside the two
// subtrees the kustomize renderer walks (core GenerateGenericKustomize:
// kustomize/base and kustomize/overlays/environment). Files elsewhere are never
// rendered, so a stray file silently rots — this guard makes that a build
// failure. It does not prove a file inside those subtrees is actually applied;
// TestDeploymentManifestsAreReferenced covers that second orphan class.
func TestDeploymentTemplatesHaveNoOrphans(t *testing.T) {
	rendered := []string{
		"templates/deployment/kustomize/base/",
		"templates/deployment/kustomize/overlays/environment/",
	}
	err := fs.WalkDir(deploymentFS, "templates/deployment", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		for _, prefix := range rendered {
			if strings.HasPrefix(path, prefix) {
				return nil
			}
		}
		t.Errorf("orphan template %q is outside the rendered subtrees %v", path, rendered)
		return nil
	})
	if err != nil {
		t.Fatalf("walk deployment templates: %v", err)
	}
}

// TestDeploymentManifestsAreReferenced catches in-tree orphans: a manifest that
// renders into kustomize/base or the overlay but is not listed in that
// directory's kustomization.yaml resources. Kustomize silently ignores such a
// file, so it renders yet never reaches the cluster — exactly the half-wiring
// that would slip past the source-level guard above (e.g. adding a base
// role.yaml.tmpl without the matching `- role.yaml` entry).
func TestDeploymentManifestsAreReferenced(t *testing.T) {
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, DeploymentParameters{Health: undeclaredHealth()})
	// The helper renders the overlay under its environment name ("test").
	assertManifestsReferenced(t, filepath.Join(dir, "base"))
	assertManifestsReferenced(t, filepath.Join(dir, "overlays", "test"))
}

// assertManifestsReferenced fails if any non-empty rendered manifest in dir is
// absent from that directory's kustomization.yaml resources list. Manifests
// that render empty under the current parameters (e.g. the ServiceAccount when
// no spec is given) are conditionally absent from resources too, so they are
// skipped — content and reference are gated by the same template condition.
func assertManifestsReferenced(t *testing.T, dir string) {
	t.Helper()
	kustomization, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("read kustomization in %s: %v", dir, err)
	}
	var parsed struct {
		Resources []string `yaml:"resources"`
	}
	if err := yaml.Unmarshal(kustomization, &parsed); err != nil {
		t.Fatalf("parse kustomization in %s: %v", dir, err)
	}
	referenced := make(map[string]bool, len(parsed.Resources))
	for _, resource := range parsed.Resources {
		referenced[resource] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == "kustomization.yaml" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.TrimSpace(string(content)) == "" {
			continue
		}
		if !referenced[name] {
			t.Errorf("rendered manifest %q in %s is not in kustomization resources — kustomize drops it, so it never reaches the cluster", name, dir)
		}
	}
}

func TestDeploymentServiceAccountRendering(t *testing.T) {
	spec := &ServiceAccountSpec{
		Name:        "db-reader",
		Annotations: map[string]string{"azure.workload.identity/client-id": "00000000-0000-0000-0000-000000000000"},
		Labels:      map[string]string{"azure.workload.identity/use": "true"},
	}
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, DeploymentParameters{ServiceAccount: spec, Health: undeclaredHealth()})

	deployment, err := os.ReadFile(filepath.Join(dir, "base", "deployment.yaml"))
	if err != nil {
		t.Fatalf("read deployment: %v", err)
	}
	if !strings.Contains(string(deployment), "serviceAccountName: db-reader") {
		t.Errorf("deployment missing serviceAccountName:\n%s", deployment)
	}
	if !strings.Contains(string(deployment), `azure.workload.identity/use: "true"`) {
		t.Errorf("deployment missing workload-identity pod label:\n%s", deployment)
	}

	sa, err := os.ReadFile(filepath.Join(dir, "base", "serviceaccount.yaml"))
	if err != nil {
		t.Fatalf("read serviceaccount: %v", err)
	}
	source := string(sa)
	for _, want := range []string{
		"kind: ServiceAccount",
		"name: db-reader",
		"app.kubernetes.io/managed-by: codefly",
		`azure.workload.identity/client-id: "00000000-0000-0000-0000-000000000000"`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("serviceaccount missing %q:\n%s", want, source)
		}
	}
}

func TestDeploymentWithoutServiceAccountRendersNoSA(t *testing.T) {
	// Production passes DeploymentParameters whose ServiceAccount is a typed-nil
	// *ServiceAccountSpec (s.GoGrpc.Settings.ServiceAccount when unset). Cover
	// both the zero-value parameters and an explicit typed-nil field so a future
	// guard change can't silently regress the default path.
	for name, params := range map[string]any{
		"zero parameters":           DeploymentParameters{Health: undeclaredHealth()},
		"typed-nil service account": DeploymentParameters{ServiceAccount: (*ServiceAccountSpec)(nil), Health: undeclaredHealth()},
	} {
		t.Run(name, func(t *testing.T) {
			dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, params)

			deployment, err := os.ReadFile(filepath.Join(dir, "base", "deployment.yaml"))
			if err != nil {
				t.Fatalf("read deployment: %v", err)
			}
			if strings.Contains(string(deployment), "serviceAccountName:") {
				t.Errorf("deployment must not set serviceAccountName without a spec:\n%s", deployment)
			}
			if _, err := os.Stat(filepath.Join(dir, "base", "serviceaccount.yaml")); err == nil {
				if data, _ := os.ReadFile(filepath.Join(dir, "base", "serviceaccount.yaml")); strings.Contains(string(data), "kind: ServiceAccount") {
					t.Errorf("no SA object should render without a spec:\n%s", data)
				}
			}
		})
	}
}

func TestSettingsValidateServiceAccount(t *testing.T) {
	tests := []struct {
		name    string
		sa      *ServiceAccountSpec
		wantErr bool
	}{
		{name: "unset", sa: nil, wantErr: false},
		{name: "valid", sa: &ServiceAccountSpec{Name: "db-reader"}, wantErr: false},
		{
			name:    "annotations without name",
			sa:      &ServiceAccountSpec{Annotations: map[string]string{"azure.workload.identity/client-id": "x"}},
			wantErr: true,
		},
		{
			name:    "labels without name",
			sa:      &ServiceAccountSpec{Labels: map[string]string{"azure.workload.identity/use": "true"}},
			wantErr: true,
		},
		{name: "invalid dns name", sa: &ServiceAccountSpec{Name: "DB_Reader"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Settings{ServiceAccount: tc.sa}).Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestSettingsValidateCors(t *testing.T) {
	tests := []struct {
		name    string
		cors    CorsSpec
		wantErr bool
	}{
		{name: "empty", cors: CorsSpec{}, wantErr: false},
		{name: "allowlist", cors: CorsSpec{AllowedOrigins: []string{"https://app.example.com"}}, wantErr: false},
		{name: "allow all", cors: CorsSpec{AllowAll: true}, wantErr: false},
		{name: "wildcard origin", cors: CorsSpec{AllowedOrigins: []string{"*"}}, wantErr: true},
		{
			name:    "allowlist with headers",
			cors:    CorsSpec{AllowedOrigins: []string{"https://app.example.com"}, AllowedHeaders: []string{"Authorization"}},
			wantErr: false,
		},
		{
			name:    "allow all with allowlist",
			cors:    CorsSpec{AllowAll: true, AllowedOrigins: []string{"https://app.example.com"}},
			wantErr: true,
		},
		{
			name:    "allow all with headers",
			cors:    CorsSpec{AllowAll: true, AllowedHeaders: []string{"Authorization"}},
			wantErr: true,
		},
		{
			name:    "headers without origins",
			cors:    CorsSpec{AllowedHeaders: []string{"Authorization"}},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Settings{Cors: tc.cors}).Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestCorsSpecDeniesCrossOrigin(t *testing.T) {
	tests := []struct {
		name string
		cors CorsSpec
		want bool
	}{
		{name: "zero value", cors: CorsSpec{}, want: true},
		{name: "allow all", cors: CorsSpec{AllowAll: true}, want: false},
		{name: "allowlist", cors: CorsSpec{AllowedOrigins: []string{"https://app.example.com"}}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cors.DeniesCrossOrigin(); got != tc.want {
				t.Fatalf("DeniesCrossOrigin() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSettingsValidateRuntimeAssets(t *testing.T) {
	tests := []struct {
		name    string
		assets  []string
		wantErr bool
	}{
		{name: "unset", assets: nil, wantErr: false},
		{name: "valid dir and file", assets: []string{"routing", "config/prod.yaml"}, wantErr: false},
		{name: "escaping", assets: []string{"../secrets"}, wantErr: true},
		{name: "absolute", assets: []string{"/etc/passwd"}, wantErr: true},
		{name: "service root", assets: []string{"."}, wantErr: true},
		{name: "whitespace", assets: []string{"my config"}, wantErr: true},
		{name: "glob", assets: []string{"conf*"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Settings{RuntimeAssets: tc.assets}).Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

// TestDeploymentProbesNeverInventARoute keeps the two invariants that survive
// every health mode: the generic deployment contract guarantees listeners, not
// routes, so no probe may reference a route this agent made up (#58), and
// liveness must stay transport-only whatever the service declares — a semantic
// liveness probe restarts a pod whose only problem is an unreachable
// dependency, cascading one outage into many.
func TestDeploymentProbesNeverInventARoute(t *testing.T) {
	template, err := fs.ReadFile(deploymentFS, "templates/deployment/kustomize/base/deployment.yaml.tmpl")
	if err != nil {
		t.Fatalf("read deployment template: %v", err)
	}
	source := string(template)
	if strings.Contains(source, "/healthz") {
		t.Fatal("generic deployment must not require a product-specific health route")
	}
	for _, params := range map[string]DeploymentParameters{
		"transport": {Health: (*HealthSpec)(nil).Normalized()},
		"grpc":      {Health: (&HealthSpec{Mode: HealthModeGrpc}).Normalized()},
		"http":      {RestEndpoint: true, Health: (&HealthSpec{Mode: HealthModeHTTP, Path: "/healthz"}).Normalized()},
	} {
		liveness := renderContainer(t, params).LivenessProbe
		if liveness.TCPSocket == nil {
			t.Fatalf("liveness must stay transport-only, got %+v", liveness.ProbeHandler)
		}
		if liveness.TCPSocket.Port.StrVal != "grpc" {
			t.Errorf("liveness probes port %v, want the always-served grpc listener", liveness.TCPSocket.Port)
		}
	}
}

// TestDeploymentProbesRenderTheDeclaredHealthContract is the acceptance matrix:
// each declared capability renders the probe that speaks it, and a service that
// declares nothing keeps the transport-only probes it has always had. Startup
// and readiness follow the declaration; liveness never does.
func TestDeploymentProbesRenderTheDeclaredHealthContract(t *testing.T) {
	grpcProbe := func(service string) corev1.ProbeHandler {
		// The kubelet takes a number here, never a port name.
		handler := corev1.GRPCAction{Port: 9090}
		if service != "" {
			handler.Service = &service
		}
		return corev1.ProbeHandler{GRPC: &handler}
	}
	httpProbe := func(path, port string) corev1.ProbeHandler {
		return corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromString(port)}}
	}
	transportProbe := corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("grpc")}}

	cases := map[string]struct {
		params DeploymentParameters
		want   corev1.ProbeHandler
	}{
		"grpc only, semantic health": {
			params: DeploymentParameters{Health: (&HealthSpec{Mode: HealthModeGrpc}).Normalized()},
			want:   grpcProbe(""),
		},
		"grpc + gateway, semantic health": {
			params: DeploymentParameters{RestEndpoint: true, Health: (&HealthSpec{Mode: HealthModeGrpc}).Normalized()},
			want:   grpcProbe(""),
		},
		"named gRPC health service": {
			params: DeploymentParameters{Health: (&HealthSpec{Mode: HealthModeGrpc, GrpcService: "acme.v1.OrderService"}).Normalized()},
			want:   grpcProbe("acme.v1.OrderService"),
		},
		"legacy customized server, transport only": {
			params: DeploymentParameters{Health: (*HealthSpec)(nil).Normalized()},
			want:   transportProbe,
		},
		"explicitly declared transport only": {
			params: DeploymentParameters{Health: (&HealthSpec{Mode: HealthModeTransport}).Normalized()},
			want:   transportProbe,
		},
		"explicit HTTP health": {
			params: DeploymentParameters{
				RestEndpoint: true,
				Health:       (&HealthSpec{Mode: HealthModeHTTP, Path: "/readyz"}).Normalized(),
			},
			want: httpProbe("/readyz", "http"),
		},
		"explicit HTTP health on connect": {
			params: DeploymentParameters{
				ConnectEndpoint: true,
				Health:          (&HealthSpec{Mode: HealthModeHTTP, Path: "/readyz", Port: "connect"}).Normalized(),
			},
			want: httpProbe("/readyz", "connect"),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			container := renderContainer(t, tc.params)
			for probe, got := range map[string]corev1.ProbeHandler{
				"startupProbe":   container.StartupProbe.ProbeHandler,
				"readinessProbe": container.ReadinessProbe.ProbeHandler,
			} {
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("%s = %+v, want %+v", probe, got, tc.want)
				}
			}
		})
	}
}

// renderContainer renders the base deployment and returns the container it
// declares. Decoding is strict against the real Kubernetes API types, so a
// probe this agent renders with a misspelled field, a wrong type (the kubelet
// takes a number for a gRPC probe's port, never a port name), or a handler the
// schema does not know fails here rather than at apply time.
func renderContainer(t *testing.T, params DeploymentParameters) corev1.Container {
	t.Helper()
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, params)
	rendered, err := os.ReadFile(filepath.Join(dir, "base", "deployment.yaml"))
	if err != nil {
		t.Fatalf("read deployment: %v", err)
	}
	var deployment appsv1.Deployment
	if err := k8syaml.UnmarshalStrict(rendered, &deployment); err != nil {
		t.Fatalf("rendered deployment is not a valid Deployment: %v\n%s", err, rendered)
	}
	containers := deployment.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("want exactly one container, got %d:\n%s", len(containers), rendered)
	}
	container := containers[0]
	for name, probe := range map[string]*corev1.Probe{
		"startupProbe":   container.StartupProbe,
		"readinessProbe": container.ReadinessProbe,
		"livenessProbe":  container.LivenessProbe,
	} {
		if probe == nil {
			t.Fatalf("container has no %s:\n%s", name, rendered)
		}
	}
	return container
}

// TestCreatePreservesAnExistingHealthDeclaration pins the rule that separates a
// new service from a customized one. Load fills Settings from the service's own
// service.codefly.yaml before Create runs and CreateResponse writes them back,
// so overwriting here would rewrite a customized service's transport-only
// declaration into one asserting a gRPC health service it may never register —
// and every probe would then fail forever.
func TestCreatePreservesAnExistingHealthDeclaration(t *testing.T) {
	for name, tc := range map[string]struct {
		existing *HealthSpec
		want     HealthMode
	}{
		"undeclared gets the scaffold's capability": {existing: nil, want: HealthModeGrpc},
		"declared transport-only is preserved":      {existing: &HealthSpec{Mode: HealthModeTransport}, want: HealthModeTransport},
		"declared http is preserved":                {existing: &HealthSpec{Mode: HealthModeHTTP, Path: "/readyz"}, want: HealthModeHTTP},
	} {
		t.Run(name, func(t *testing.T) {
			builder := NewBuilder(NewService())
			builder.GoGrpc.Settings.Health = tc.existing
			builder.declareHealthCapability()
			if got := builder.GoGrpc.Settings.Health.Mode; got != tc.want {
				t.Errorf("health mode after Create = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDeploymentRenderingRefusesAnUnresolvedHealthContract keeps the zero value
// from meaning "transport". A parameter set built without Normalized must fail
// rendering, not quietly downgrade a service's semantic probes to a TCP
// connect — the silent downgrade this whole change exists to remove.
func TestDeploymentRenderingRefusesAnUnresolvedHealthContract(t *testing.T) {
	if _, err := (HealthSpec{}).Handler(); err == nil {
		t.Fatal("an unresolved health contract must not resolve to a probe handler")
	}
	for _, mode := range []HealthMode{HealthModeTransport, HealthModeGrpc, HealthModeHTTP} {
		if _, err := (HealthSpec{Mode: mode}).Handler(); err != nil {
			t.Errorf("mode %q must render: %v", mode, err)
		}
	}
}

// TestDeploymentHealthFieldsAreQuoted proves the two interpolated health values
// cannot restructure the manifest they land in. Unquoted, a newline in either
// injects sibling keys into the probe — silently overriding its timing with a
// manifest that still parses — and an unconstrained value reaches the template
// at all only if validation lets it through.
func TestDeploymentHealthFieldsAreQuoted(t *testing.T) {
	injection := "svc\n            timeoutSeconds: 999"
	container := renderContainer(t, DeploymentParameters{
		Health: HealthSpec{Mode: HealthModeGrpc, GrpcService: injection},
	})
	probe := container.ReadinessProbe
	if probe.GRPC == nil || probe.GRPC.Service == nil || *probe.GRPC.Service != injection {
		t.Fatalf("grpc-service did not survive as a single scalar: %+v", probe.ProbeHandler)
	}
	if probe.TimeoutSeconds == 999 {
		t.Error("grpc-service injected a sibling key into the probe")
	}

	path := "/readyz #frag"
	container = renderContainer(t, DeploymentParameters{
		RestEndpoint: true,
		Health:       HealthSpec{Mode: HealthModeHTTP, Path: path, Port: "http"},
	})
	if got := container.ReadinessProbe.HTTPGet.Path; got != path {
		t.Errorf("path rendered as %q, want %q — an unquoted %q truncates it into a YAML comment", got, path, "#")
	}
}

// TestSettingsValidateHealth covers the declarations that must fail before they
// reach a cluster: an unsupported mode, a half-written block, a route on a
// listener the process never binds (#78), and fields belonging to another mode
// — each of which would otherwise render a probe that can never pass.
func TestSettingsValidateHealth(t *testing.T) {
	tests := []struct {
		name     string
		settings Settings
		wantErr  string
	}{
		{name: "undeclared", settings: Settings{}},
		{name: "transport", settings: Settings{Health: &HealthSpec{Mode: HealthModeTransport}}},
		{name: "grpc", settings: Settings{Health: &HealthSpec{Mode: HealthModeGrpc}}},
		{
			name:     "grpc with service name",
			settings: Settings{Health: &HealthSpec{Mode: HealthModeGrpc, GrpcService: "acme.v1.OrderService"}},
		},
		{
			name:     "http on the rest listener",
			settings: Settings{RestEndpoint: true, Health: &HealthSpec{Mode: HealthModeHTTP, Path: "/readyz"}},
		},
		{
			name:     "http on the connect listener",
			settings: Settings{ConnectEndpoint: true, Health: &HealthSpec{Mode: HealthModeHTTP, Path: "/readyz", Port: "connect"}},
		},
		{
			name:     "unsupported mode",
			settings: Settings{Health: &HealthSpec{Mode: "sql"}},
			wantErr:  `unsupported mode "sql"`,
		},
		{
			name:     "missing mode",
			settings: Settings{Health: &HealthSpec{Path: "/readyz"}},
			wantErr:  "mode is required",
		},
		{
			name:     "http without a path",
			settings: Settings{RestEndpoint: true, Health: &HealthSpec{Mode: HealthModeHTTP}},
			wantErr:  "requires an absolute path",
		},
		{
			name:     "http with a relative path",
			settings: Settings{RestEndpoint: true, Health: &HealthSpec{Mode: HealthModeHTTP, Path: "readyz"}},
			wantErr:  "requires an absolute path",
		},
		{
			name:     "http on an unbound rest listener",
			settings: Settings{Health: &HealthSpec{Mode: HealthModeHTTP, Path: "/readyz"}},
			wantErr:  "requires rest-endpoint: true",
		},
		{
			name:     "http on an unbound connect listener",
			settings: Settings{RestEndpoint: true, Health: &HealthSpec{Mode: HealthModeHTTP, Path: "/readyz", Port: "connect"}},
			wantErr:  "requires connect-endpoint: true",
		},
		{
			name:     "http on an unknown listener",
			settings: Settings{RestEndpoint: true, Health: &HealthSpec{Mode: HealthModeHTTP, Path: "/readyz", Port: "grpc"}},
			wantErr:  `port "grpc" is not a listener`,
		},
		{
			name:     "grpc with an http route",
			settings: Settings{RestEndpoint: true, Health: &HealthSpec{Mode: HealthModeGrpc, Path: "/readyz"}},
			wantErr:  "takes no path or port",
		},
		{
			name:     "http with a grpc service name",
			settings: Settings{RestEndpoint: true, Health: &HealthSpec{Mode: HealthModeHTTP, Path: "/readyz", GrpcService: "acme.v1.OrderService"}},
			wantErr:  "takes no grpc-service",
		},
		{
			name:     "grpc service name with a newline",
			settings: Settings{Health: &HealthSpec{Mode: HealthModeGrpc, GrpcService: "svc\n  timeoutSeconds: 999"}},
			wantErr:  "fully qualified protobuf service name",
		},
		{
			name:     "grpc service name with a slash",
			settings: Settings{Health: &HealthSpec{Mode: HealthModeGrpc, GrpcService: "acme.v1.Order/Check"}},
			wantErr:  "fully qualified protobuf service name",
		},
		{
			name:     "http path with a newline",
			settings: Settings{RestEndpoint: true, Health: &HealthSpec{Mode: HealthModeHTTP, Path: "/readyz\n  periodSeconds: 1"}},
			wantErr:  "must be a URL path",
		},
		{
			name:     "http path with a comment marker",
			settings: Settings{RestEndpoint: true, Health: &HealthSpec{Mode: HealthModeHTTP, Path: "/readyz #frag"}},
			wantErr:  "must be a URL path",
		},
		{
			name:     "transport with a grpc service name",
			settings: Settings{Health: &HealthSpec{Mode: HealthModeTransport, GrpcService: "acme.v1.OrderService"}},
			wantErr:  "takes no grpc-service, path, or port",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.settings.validateHealth()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected validation error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected validation error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestDeploymentPortsMatchDeclaredEndpoints pins the container/Service port set
// to the listeners the process actually binds: grpc always, http only with the
// REST endpoint, connect only with the Connect endpoint. A port advertised for
// an unserved listener is the #78 failure — a Service routing to a dead port
// and, when probed, a pod that restart-loops forever.
func TestDeploymentPortsMatchDeclaredEndpoints(t *testing.T) {
	type portCheck struct {
		token   string
		wantFor func(DeploymentParameters) bool
	}
	// Substrings unique to each listener's port block across deployment.yaml
	// (containerPort) and service.yaml (Service port).
	checks := []portCheck{
		{"containerPort: 9090", func(DeploymentParameters) bool { return true }},
		{"name: grpc-port", func(DeploymentParameters) bool { return true }},
		{"containerPort: 8080", func(p DeploymentParameters) bool { return p.RestEndpoint }},
		{"name: http-port", func(p DeploymentParameters) bool { return p.RestEndpoint }},
		{"containerPort: 8081", func(p DeploymentParameters) bool { return p.ConnectEndpoint }},
		{"name: connect-port", func(p DeploymentParameters) bool { return p.ConnectEndpoint }},
	}
	cases := map[string]DeploymentParameters{
		"grpc only":      {Health: undeclaredHealth()},
		"grpc + rest":    {RestEndpoint: true, Health: undeclaredHealth()},
		"grpc + connect": {ConnectEndpoint: true, Health: undeclaredHealth()},
		"all":            {RestEndpoint: true, ConnectEndpoint: true, Health: undeclaredHealth()},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, params)
			deployment, err := os.ReadFile(filepath.Join(dir, "base", "deployment.yaml"))
			if err != nil {
				t.Fatalf("read deployment: %v", err)
			}
			service, err := os.ReadFile(filepath.Join(dir, "base", "service.yaml"))
			if err != nil {
				t.Fatalf("read service: %v", err)
			}
			rendered := string(deployment) + string(service)
			for _, check := range checks {
				got := strings.Contains(rendered, check.token)
				if want := check.wantFor(params); got != want {
					t.Errorf("port token %q present=%v, want %v\n%s", check.token, got, want, rendered)
				}
			}
		})
	}
}

// renderOverlayWithConfigMap renders the deployment templates the way
// DeployKustomize does in production — core wraps the agent's parameters in
// services.DeploymentParameters and it is that wrapper the overlay templates
// range over — but with a caller-chosen ConfigMap so a hostile value can be
// driven through the real renderer. It returns the rendered destination root.
func renderOverlayWithConfigMap(t *testing.T, configMap services.EnvironmentMap) string {
	t.Helper()
	ctx := context.Background()
	identity := &resources.ServiceIdentity{
		Workspace: "workspace",
		Module:    "module",
		Name:      "example-service",
		Version:   "1.2.3",
	}
	base := &services.Base{
		Wool:     wool.Get(ctx),
		Identity: identity,
		Information: &services.Information{
			Service: resources.ToServiceWithCase(identity),
			Module:  resources.ToModuleWithCase(identity),
		},
	}
	base.SetDockerImage(resources.NewDockerImage("example/service:1.2.3"))
	builder := &services.BuilderWrapper{Base: base}
	base.Builder = builder

	destination := t.TempDir()
	deployment := &builderv0.KubernetesDeployment{
		Namespace:   "codefly-test",
		Destination: destination,
		Profile:     builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1,
	}
	params := services.DeploymentParameters{
		ConfigMap:  configMap,
		Parameters: DeploymentParameters{Health: undeclaredHealth()},
	}
	if err := builder.KustomizeDeploy(ctx, &basev0.Environment{Name: "test"}, deployment, deploymentFS, params); err != nil {
		t.Fatalf("render kustomize templates: %v", err)
	}
	return destination
}

// readRenderedConfigMap parses the rendered overlay ConfigMap and returns both
// its raw bytes and its decoded data map.
func readRenderedConfigMap(t *testing.T, destination string) (string, map[string]string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(destination, "overlays", "test", "configmap.yaml"))
	if err != nil {
		t.Fatalf("read rendered configmap: %v", err)
	}
	var parsed struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("rendered configmap is not valid YAML: %v\n%s", err, raw)
	}
	return string(raw), parsed.Data
}

// TestConfigMapEscapesHostileValues pins the ConfigMap value escaping. The
// template used to interpolate the raw value between two literal quotes, so a
// configuration value carrying a double quote, a backslash or a newline — a
// JSON document is the ordinary case — rendered a broken manifest and failed
// the whole gitops render with "did not find expected key". The value is now
// emitted through printf %q, whose escaping is a valid YAML double-quoted
// scalar, so the value survives the round trip byte for byte.
func TestConfigMapEscapesHostileValues(t *testing.T) {
	hostile := "{\"kind\":\"json\",\"path\":\"C:\\\\tmp\"}\nsecond line\twith a tab"
	destination := renderOverlayWithConfigMap(t, services.EnvironmentMap{"CODEFLY_HOSTILE_VALUE": hostile})
	_, data := readRenderedConfigMap(t, destination)
	got, ok := data["CODEFLY_HOSTILE_VALUE"]
	if !ok {
		t.Fatalf("rendered configmap has no CODEFLY_HOSTILE_VALUE: %v", data)
	}
	if got != hostile {
		t.Errorf("configmap value did not survive the round trip:\n got %q\nwant %q", got, hostile)
	}
}

// TestConfigMapPlainValueRendersUnchanged holds the escaping to a no-op for the
// ordinary value: %q must produce exactly the quoted form the template emitted
// before, so moving to it churns no existing manifest.
func TestConfigMapPlainValueRendersUnchanged(t *testing.T) {
	destination := renderOverlayWithConfigMap(t, services.EnvironmentMap{"CODEFLY_TEST_VALUE": "abc"})
	raw, data := readRenderedConfigMap(t, destination)
	if !strings.Contains(raw, "\n  CODEFLY_TEST_VALUE: \"abc\"\n") {
		t.Errorf("plain value must render as an unchanged quoted scalar:\n%s", raw)
	}
	if data["CODEFLY_TEST_VALUE"] != "abc" {
		t.Errorf("plain value round trip: got %q", data["CODEFLY_TEST_VALUE"])
	}
}

// TestSecretTemplateValuesAreBase64 records why the sibling Secret template
// keeps its literal quotes: core fills SecretMap through
// EnvsAsSecretData → ValueAsEncodedString, so every value is standard base64
// and cannot contain a quote, a backslash or a newline. The assertion is on the
// encoding contract rather than on the template text, so the day a value stops
// being encoded this fails rather than silently reopening the same defect.
func TestSecretTemplateValuesAreBase64(t *testing.T) {
	hostile := "a\"b\\c\nd"
	encoded, err := services.EnvsAsSecretData(&resources.EnvironmentVariable{Key: "CODEFLY_TEST_SECRET", Value: hostile})
	if err != nil {
		t.Fatalf("encode secret data: %v", err)
	}
	value := encoded["CODEFLY_TEST_SECRET"]
	if strings.ContainsAny(value, "\"\\\n") {
		t.Fatalf("secret values are no longer safe inside a literal-quoted scalar: %q", value)
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("secret value is not base64: %v", err)
	}
	if string(decoded) != hostile {
		t.Errorf("secret value round trip: got %q want %q", decoded, hostile)
	}
}
