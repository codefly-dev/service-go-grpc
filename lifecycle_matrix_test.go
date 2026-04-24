package main_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/runners/testmatrix"
)

// TestGoGrpcLifecycle_Matrix exercises the go-grpc plugin's execution
// parity across native, nix, and docker backends using the shared
// ForEachEnvironment harness. Each sub-test asserts that `go version`
// runs and returns a recognizable Go version string, which is the
// minimum mode-consistency guarantee: the Go toolchain MUST exist in
// every backend the plugin claims to support.
//
// Backends unavailable on the host are skipped, so a dev without Docker
// still validates native + nix in CI-independent runs.
func TestGoGrpcLifecycle_Matrix(t *testing.T) {
	dir, err := os.MkdirTemp("", "gogrpc-matrix-*")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(dir)

	// golang:1.26-alpine has the toolchain + /bin/sh, ~370MB. Matches
	// what the agent uses for container mode at runtime.
	img := &resources.DockerImage{Name: "golang", Tag: "1.26-alpine"}

	testmatrix.ForEachEnvironment(t, dir,
		func(t *testing.T, env runners.RunnerEnvironment) {
			proc, err := env.NewProcess("go", "version")
			if err != nil {
				t.Fatalf("NewProcess: %v", err)
			}
			var buf bytes.Buffer
			proc.WithOutput(&buf)
			if err := proc.Run(context.Background()); err != nil {
				t.Fatalf("go version failed: %v", err)
			}
			out := buf.String()
			if !strings.Contains(out, "go version") {
				t.Fatalf("expected Go version string, got %q", out)
			}
		},
		testmatrix.WithDockerImage(img),
	)
}
