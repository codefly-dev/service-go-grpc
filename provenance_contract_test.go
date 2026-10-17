package main

import (
	"context"
	"slices"
	"testing"

	"github.com/codefly-dev/core/agents/contract"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
)

// TestAdvertisementAdoptsItsOperationContracts: a host requires the live
// contract capability before it trusts an optional behaviour, and core never
// advertises one on an agent's behalf — so each capability here is this agent's
// own claim, and every claim must stay true.
//
//   - deployment-composition-provenance/v1: the deployment path is core's
//     DeployKustomize, which judges dependency edges with the request's
//     composition provenance (core#752).
//   - runtime-started-artifact/v1: Runtime.Start reports the executable it
//     launched (core#796). This asserts only that the claim is made; that Start
//     really reports is held by the start-path tests in
//     runtime_started_artifact_test.go, one of which asserts this capability
//     beside the behaviour so the two cannot drift apart.
//
// The base contract this extends must survive both.
func TestAdvertisementAdoptsItsOperationContracts(t *testing.T) {
	info, err := NewService().GetAgentInformation(context.Background(), &agentv0.AgentInformationRequest{})
	if err != nil {
		t.Fatalf("GetAgentInformation: %v", err)
	}
	for _, capability := range []string{contract.DeploymentCompositionProvenance, contract.RuntimeStartedArtifact} {
		if !slices.Contains(info.GetContract().GetCapabilities(), capability) {
			t.Fatalf("contract capabilities %v do not advertise %s", info.GetContract().GetCapabilities(), capability)
		}
	}
	if err := contract.Check(info.GetContract(), contract.ContainerRecoveryScope); err != nil {
		t.Fatalf("the base contract is no longer satisfied: %v", err)
	}
}
