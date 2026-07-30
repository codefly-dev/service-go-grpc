package main

import (
	"io/fs"
	"strings"
	"testing"

	agenttesting "github.com/codefly-dev/core/agents/testing"
)

func TestDeploymentTemplates(t *testing.T) {
	agenttesting.AssertKustomizeTemplates(t, deploymentFS, nil)
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

func TestAgentVersion(t *testing.T) {
	if agent.Version != "0.1.27" {
		t.Fatalf("agent version = %q, want 0.1.27", agent.Version)
	}
}
