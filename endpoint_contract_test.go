package main

import (
	"testing"

	"buf.build/go/protovalidate"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/stretchr/testify/require"
)

// TestRuntimeContractAcceptsInternalEndpoints pins the current Core endpoint
// contract into the agent binary. Runtime Load responses cross this validation
// boundary before the CLI can enforce a module allow-list, so an older embedded
// descriptor makes correctly private cross-module services impossible to run.
func TestRuntimeContractAcceptsInternalEndpoints(t *testing.T) {
	validator, err := protovalidate.New()
	require.NoError(t, err)

	err = validator.Validate(&runtimev0.LoadResponse{
		Endpoints: []*basev0.Endpoint{{
			Name:         "grpc",
			Service:      "work-coordinator",
			Module:       "coordination",
			Api:          "grpc",
			Visibility:   "internal",
			AllowModules: []string{"mind"},
		}},
	})
	require.NoError(t, err)
}
