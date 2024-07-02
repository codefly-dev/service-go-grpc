package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/codefly-dev/core/agents"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"path"
	"testing"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"

	"github.com/codefly-dev/core/resources"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
)

func TestCreateToRunNative(t *testing.T) {
	if languages.HasGoRuntime(nil) {
		testCreateToRun(t, resources.NewRuntimeContextNative())
	}
}

func TestCreateToRunDocker(t *testing.T) {
	t.Skip("skipping: fix this garbage")
	testCreateToRun(t, resources.NewRuntimeContextContainer())
}

func testCreateToRun(t *testing.T, runtimeContext *basev0.RuntimeContext) {
	wool.SetGlobalLogLevel(wool.DEBUG)
	agents.LogToConsole()

	ctx := context.Background()

	var err error
	tmpDir := t.TempDir()

	workspace := &resources.Workspace{Name: "test"}

	service := &resources.Service{Name: "svc", Module: "mod", Version: "0.0.0"}
	err = service.SaveAtDir(ctx, path.Join(tmpDir, service.Unique()))
	require.NoError(t, err)

	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Version:             service.Version,
		Module:              service.Module,
		Workspace:           workspace.Name,
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: service.Unique(),
	}
	env := resources.LocalEnvironment()

	builder := NewBuilder()

	resp, err := builder.Load(ctx, &builderv0.LoadRequest{Identity: identity, CreationMode: &builderv0.CreationMode{Communicate: false}})
	require.NoError(t, err)
	require.NotNil(t, resp)

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	// Now run it
	runtime := NewRuntime()

	_, err = runtime.Load(ctx, &runtimev0.LoadRequest{Identity: identity, Environment: shared.Must(env.Proto()), DisableCatch: true})
	require.NoError(t, err)

	require.Equal(t, 2, len(runtime.Endpoints))

	// Create temporary network mappings
	networkManager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	networkManager.WithTemporaryPorts()

	networkMappings, err := networkManager.GenerateNetworkMappings(ctx, env, workspace, service, runtime.Endpoints)
	require.NoError(t, err)
	require.NotNil(t, networkMappings)
	require.Equal(t, 2, len(networkMappings))

	init, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          runtimeContext,
		ProposedNetworkMappings: networkMappings})
	require.NoError(t, err)
	require.NotNil(t, init)

	testRun(t, runtime, ctx, identity, networkMappings)
	//
	//_, err = runtime.Stop(ctx, &runtimev0.StopRequest{})
	//require.NoError(t, err)
	//
	//// Check that the runner is stopped
	//time.Sleep(2 * time.Second)
	//running, err := runtime.runner.IsRunning(ctx)
	//require.NoError(t, err)
	//require.False(t, running)
	//
	//testNoApi(t, runtime, ctx, networkMappings)

	// Running again should work
	// testRun(t, runtime, ctx, identity, networkMappings)

	// Test
	//test, err := runtime.Test(ctx, &runtimev0.TestRequest{})
	//require.NoError(t, err)
	//require.Equal(t, runtimev0.TestStatus_SUCCESS, test.Status.State)

	_, _ = runtime.Destroy(ctx, &runtimev0.DestroyRequest{})

}

func testRun(t *testing.T, runtime *Runtime, ctx context.Context, identity *basev0.ServiceIdentity, networkMappings []*basev0.NetworkMapping) {

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, networkMappings, runtime.RestEndpoint, resources.NewNativeNetworkAccess())
	require.NoError(t, err)

	_, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)

	client := http.Client{Timeout: 200 * time.Millisecond}
	// Loop and wait for 1 seconds up to do a HTTP request to localhost with /version path
	tries := 0
	for {
		if tries > 10 {
			t.Fatal("too many tries")
		}
		time.Sleep(time.Second)

		// HTTP
		response, err := client.Get(fmt.Sprintf("%s/version", instance.Address))
		if err != nil {
			tries++
			continue
		}
		if response.StatusCode != http.StatusOK {
			tries++
			continue
		}

		defer response.Body.Close()

		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)

		var data map[string]interface{}
		err = json.Unmarshal(body, &data)
		require.NoError(t, err)

		version, ok := data["version"].(string)
		require.True(t, ok)
		require.Equal(t, identity.Version, version)
		return
	}
}

func testNoApi(t *testing.T, runtime *Runtime, ctx context.Context, networkMappings []*basev0.NetworkMapping) {
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, networkMappings, runtime.RestEndpoint, resources.NewNativeNetworkAccess())
	require.NoError(t, err)

	client := http.Client{Timeout: 200 * time.Millisecond}
	// HTTP
	response, err := client.Get(fmt.Sprintf("%s/version", instance.Address))
	if err != nil {
		return
	}

	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	fmt.Println(string(body))

	t.Fatal("should not have reached here")

}
