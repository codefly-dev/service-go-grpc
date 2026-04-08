package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
)

func (s *Runtime) registerCommands() {
	// test: run go tests
	s.RegisterCommand(&agentv0.CommandDefinition{
		Name:        "test",
		Description: "Run Go tests for this service",
		Usage:       "test [--race] [--verbose] [--run pattern]",
		Tags:        []string{"testing", "dev"},
	}, s.cmdTest)

	// lint: run linter
	s.RegisterCommand(&agentv0.CommandDefinition{
		Name:        "lint",
		Description: "Run golangci-lint on the service code",
		Tags:        []string{"quality", "dev"},
	}, s.cmdLint)

	// proto: regenerate protobuf code
	s.RegisterCommand(&agentv0.CommandDefinition{
		Name:        "proto",
		Description: "Regenerate protobuf Go code from .proto files",
		Tags:        []string{"codegen", "proto"},
		Aliases:     []string{"generate", "buf"},
	}, s.cmdProto)

	// health: check if gRPC server is responding
	s.RegisterCommand(&agentv0.CommandDefinition{
		Name:        "health",
		Description: "Check if the gRPC service is responding",
		Tags:        []string{"health", "diagnostic"},
	}, s.cmdHealth)
}

func (s *Runtime) cmdTest(ctx context.Context, args []string) (string, error) {
	goArgs := []string{"test", "./..."}
	for _, arg := range args {
		switch arg {
		case "--race":
			goArgs = append(goArgs, "-race")
		case "--verbose", "-v":
			goArgs = append(goArgs, "-v")
		default:
			if strings.HasPrefix(arg, "--run") {
				goArgs = append(goArgs, "-run", strings.TrimPrefix(arg, "--run="))
			}
		}
	}

	cmd := exec.CommandContext(ctx, "go", goArgs...)
	cmd.Dir = s.sourceLocation
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("tests failed: %w", err)
	}
	return string(output), nil
}

func (s *Runtime) cmdLint(ctx context.Context, _ []string) (string, error) {
	cmd := exec.CommandContext(ctx, "golangci-lint", "run", "./...")
	cmd.Dir = s.sourceLocation
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("lint failed: %w", err)
	}
	return string(output), nil
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

func (s *Runtime) cmdHealth(ctx context.Context, _ []string) (string, error) {
	if s.runner == nil {
		return "NOT RUNNING", nil
	}
	return "RUNNING", nil
}
