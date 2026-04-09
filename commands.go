package main

import (
	"context"
	"fmt"
	"os/exec"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
)

// registerCommands registers agent-specific commands.
// NOTE: test and lint are standard Runtime RPCs — don't duplicate here.
// Only register commands that are unique to this agent type.
func (s *Runtime) registerCommands() {
	s.RegisterCommand(&agentv0.CommandDefinition{
		Name:        "proto",
		Description: "Regenerate protobuf Go code from .proto files via buf",
		Tags:        []string{"codegen", "proto"},
		Aliases:     []string{"generate", "buf"},
	}, s.cmdProto)

	s.RegisterCommand(&agentv0.CommandDefinition{
		Name:        "health",
		Description: "Check if the gRPC service is responding",
		Tags:        []string{"health", "diagnostic"},
	}, s.cmdHealth)
}

func (s *Runtime) cmdProto(ctx context.Context, _ []string) (string, error) {
	cmd := exec.CommandContext(ctx, "buf", "generate")
	cmd.Dir = s.sourceLocation + "/../proto"
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("proto generation failed: %w", err)
	}
	return "Proto code regenerated successfully\n" + string(output), nil
}

func (s *Runtime) cmdHealth(_ context.Context, _ []string) (string, error) {
	if s.runner == nil {
		return "NOT RUNNING", nil
	}
	return "RUNNING", nil
}
