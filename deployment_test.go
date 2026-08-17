package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agenttesting "github.com/codefly-dev/core/agents/testing"
)

func TestDeploymentTemplates(t *testing.T) {
	agenttesting.AssertKustomizeTemplates(t, deploymentFS, nil)
}

// TestDeploymentTemplatesHaveNoOrphans fails if any file under
// templates/deployment sits outside the two subtrees the kustomize renderer
// walks (core GenerateGenericKustomize: kustomize/base and
// kustomize/overlays/environment). Files elsewhere are never rendered, so a
// stray template silently rots — this guard makes that a build failure.
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

func TestDeploymentServiceAccountRendering(t *testing.T) {
	spec := &ServiceAccountSpec{
		Name:        "db-reader",
		Annotations: map[string]string{"azure.workload.identity/client-id": "00000000-0000-0000-0000-000000000000"},
		Labels:      map[string]string{"azure.workload.identity/use": "true"},
	}
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, spec)

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
	// Production passes a typed-nil *ServiceAccountSpec (s.GoGrpc.Settings.
	// ServiceAccount when unset), not an untyped nil — cover both so a future
	// guard change can't silently regress the default path.
	for name, params := range map[string]any{
		"untyped nil": nil,
		"typed nil":   (*ServiceAccountSpec)(nil),
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

func TestDeploymentProbesRequireOnlyTheDeclaredListener(t *testing.T) {
	template, err := fs.ReadFile(deploymentFS, "templates/deployment/kustomize/base/deployment.yaml.tmpl")
	if err != nil {
		t.Fatalf("read deployment template: %v", err)
	}
	source := string(template)
	if strings.Contains(source, "/healthz") {
		t.Fatal("generic deployment must not require a product-specific health route")
	}
	if count := strings.Count(source, "tcpSocket:"); count != 3 {
		t.Fatalf("transport probes = %d, want startup, readiness, and liveness", count)
	}
}
