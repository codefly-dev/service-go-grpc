package main

import (
	"testing"

	agenttesting "github.com/codefly-dev/core/agents/testing"
)

func TestDeploymentTemplates(t *testing.T) {
	agenttesting.AssertKustomizeTemplates(t, deploymentFS, nil)
}

func TestAgentVersion(t *testing.T) {
	if agent.Version != "0.1.25" {
		t.Fatalf("agent version = %q, want 0.1.25", agent.Version)
	}
}
