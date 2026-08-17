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
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, nil)

	deployment, err := os.ReadFile(filepath.Join(dir, "base", "deployment.yaml"))
	if err != nil {
		t.Fatalf("read deployment: %v", err)
	}
	if strings.Contains(string(deployment), "serviceAccountName:") {
		t.Errorf("deployment must not set serviceAccountName without a spec:\n%s", deployment)
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
