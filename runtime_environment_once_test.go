package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// TestStartedProcessReceivesEachConfigurationVariableOnce holds that the
// service's configuration reaches its process exactly once. A started process
// receives the runner environment's entries and then Start's own, so the set
// is easy to deliver twice: Init used to put it on the runner environment and
// Start to hand it again. A core that counts every entry against the 512 KiB
// process-environment limit (before v0.19.12) then refused a service whose
// configuration was only half that size (service-go#162).
func TestStartedProcessReceivesEachConfigurationVariableOnce(t *testing.T) {
	if !languages.HasGoRuntime(nil) {
		t.Skip("native go toolchain not available")
	}
	ctx := context.Background()
	runtimeContext := resources.NewRuntimeContextNative()
	runtime, mappings, workspacePath := loadNamedEndpointRuntime(t, runtimeContext)
	t.Cleanup(func() {
		_, _ = runtime.Stop(ctx, &runtimev0.StopRequest{})
	})

	workspaceConfiguration := &basev0.Configuration{
		Origin:         resources.ConfigurationWorkspace,
		RuntimeContext: runtimeContext,
		Infos: []*basev0.ConfigurationInformation{{
			Name:                "robin",
			ConfigurationValues: []*basev0.ConfigurationValue{{Key: "chat", Value: "bound"}},
		}},
	}
	// No runner is bound beforehand, so Init creates it exactly as in production.
	init, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          runtimeContext,
		ProposedNetworkMappings: mappings,
		WorkspaceConfigurations: []*basev0.Configuration{workspaceConfiguration},
	})
	require.NoError(t, err)
	require.Equal(t, runtimev0.InitStatus_READY, init.GetStatus().GetState(), init.GetStatus().GetMessage())

	// What Start hands the process: the configuration, each key once.
	started, err := runtime.EnvironmentVariables.All()
	require.NoError(t, err)
	seen := map[string]int{}
	configured := 0
	for _, env := range started {
		seen[env.Key]++
		if strings.HasPrefix(env.Key, resources.WorkspaceConfigurationPrefix+"__") {
			configured++
		}
	}
	for key, count := range seen {
		require.Equalf(t, 1, count, "Start hands %s %d times", key, count)
	}
	require.Equal(t, 1, configured, "the workspace configuration is in Start's set")

	// What the process inherits beneath that set: a process from the runner
	// environment, with nothing of its own, reports the environment it was given.
	dump := filepath.Join(workspacePath, "environ")
	proc, err := runtime.RunnerEnvironment.Env().NewProcess("sh", "-c", `env > "$0"`, dump)
	require.NoError(t, err)
	require.NoError(t, proc.Run(ctx))
	inherited, err := os.ReadFile(dump)
	require.NoError(t, err)
	for _, line := range strings.Split(string(inherited), "\n") {
		require.Falsef(t, strings.HasPrefix(line, "CODEFLY__"),
			"the runner environment carries %q, so the process would receive it twice", line)
	}
}
