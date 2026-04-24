package main

import (
	"context"
	"encoding/json"
	"fmt"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"

	"github.com/codefly-dev/core/resources"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
)

func TestSetRuntimeContextNix(t *testing.T) {
	ctx := context.Background()

	runtime := NewRuntime(NewService())
	runtime.Base.Runtime.RuntimeContext = resources.NewRuntimeContextNative() // start with native

	err := runtime.SetRuntimeContext(ctx, resources.NewRuntimeContextNix())
	require.NoError(t, err)
	require.Equal(t, resources.RuntimeContextNix, runtime.Base.Runtime.RuntimeContext.Kind)
	require.True(t, runtime.Base.Runtime.IsNixRuntime())
	require.False(t, runtime.Base.Runtime.IsContainerRuntime())
	require.False(t, runtime.Base.Runtime.IsNativeRuntime())
}

func TestSetRuntimeContextNative(t *testing.T) {
	ctx := context.Background()

	runtime := NewRuntime(NewService())

	err := runtime.SetRuntimeContext(ctx, resources.NewRuntimeContextNative())
	require.NoError(t, err)

	if languages.HasGoRuntime(nil) {
		require.Equal(t, resources.RuntimeContextNative, runtime.Base.Runtime.RuntimeContext.Kind)
	} else {
		require.Equal(t, resources.RuntimeContextContainer, runtime.Base.Runtime.RuntimeContext.Kind)
	}
}

func TestCreateToRunNative(t *testing.T) {
	if languages.HasGoRuntime(nil) {
		testCreateToRun(t, resources.NewRuntimeContextNative(), false)
	}
}

func TestCreateToRunDocker(t *testing.T) {
	testCreateToRun(t, resources.NewRuntimeContextContainer(), false)
}

func TestCreateToRunWithConnectNative(t *testing.T) {
	// Skip until core is published with standards.CONNECT support.
	// The scaffolded service depends on published core which doesn't
	// have the CONNECT constant yet.
	t.Skip("requires published core with standards.CONNECT")
	if languages.HasGoRuntime(nil) {
		testCreateToRun(t, resources.NewRuntimeContextNative(), true)
	}
}

func testCreateToRun(t *testing.T, runtimeContext *basev0.RuntimeContext, withConnect bool) {
	wool.SetGlobalLogLevel(wool.DEBUG)
	ctx := context.Background()

	var err error
	tmpDir := t.TempDir()

	workspace := &resources.Workspace{Name: "test"}

	service := &resources.Service{Name: "svc", Version: "0.0.0"}
	err = service.SaveAtDir(ctx, path.Join(tmpDir, fmt.Sprintf("mod/%s", service.Name)))
	require.NoError(t, err)
	service.WithModule("mod")
	mod := &resources.Module{Name: "mod"}

	err = mod.SaveToDir(ctx, path.Join(tmpDir, "mod"))
	require.NoError(t, err)

	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Version:             service.Version,
		Module:              "mod",
		Workspace:           workspace.Name,
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: fmt.Sprintf("mod/%s", service.Name),
	}
	env := resources.LocalEnvironment()

	// randomize
	env.NamingScope = strconv.Itoa(time.Now().Second())

	builder := NewBuilder(NewService())

	resp, err := builder.Load(ctx, &builderv0.LoadRequest{Identity: identity, CreationMode: &builderv0.CreationMode{Communicate: false}})
	require.NoError(t, err)
	require.NotNil(t, resp)

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	// For Connect tests, enable Connect and re-create endpoints + save
	if withConnect {
		builder.GoGrpc.Settings.ConnectEndpoint = true
		builder.Endpoints = nil // reset
		err = builder.CreateEndpoints(ctx)
		require.NoError(t, err)
		require.Equal(t, 3, len(builder.Endpoints), "expected grpc+rest+connect endpoints")

		// Re-save with connect endpoint
		svcDir := path.Join(tmpDir, "mod/svc")
		reloadedSvc, err := resources.LoadServiceFromDir(ctx, svcDir)
		require.NoError(t, err)
		reloadedSvc.Endpoints, err = resources.FromProtoEndpoints(builder.Endpoints...)
		require.NoError(t, err)
		reloadedSvc.Spec["connect-endpoint"] = true
		err = reloadedSvc.Save(ctx)
		require.NoError(t, err)
	}

	// Now run it
	runtime := NewRuntime(NewService())

	_, err = runtime.Load(ctx, &runtimev0.LoadRequest{
		Identity:     identity,
		Environment:  shared.Must(env.Proto()),
		DisableCatch: true})
	require.NoError(t, err)

	expectedEndpoints := 2 // grpc + rest
	if withConnect {
		expectedEndpoints = 3 // grpc + rest + connect
	}
	require.Equal(t, expectedEndpoints, len(runtime.Endpoints))

	// Create temporary network mappings
	networkManager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	networkManager.WithTemporaryPorts()

	networkMappings, err := networkManager.GenerateNetworkMappings(ctx, env, workspace, runtime.Identity, runtime.Endpoints)
	require.NoError(t, err)
	require.NotNil(t, networkMappings)
	require.Equal(t, expectedEndpoints, len(networkMappings))

	init, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          runtimeContext,
		ProposedNetworkMappings: networkMappings})
	require.NoError(t, err)
	require.NotNil(t, init)

	defer func() {
		_, _ = runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	}()

	testRun(t, runtime, ctx, identity, networkMappings)

	// Test Connect endpoint (if configured)
	testConnectEndpoint(t, runtime, ctx, identity, networkMappings)

	// Test
	test, err := runtime.Test(ctx, &runtimev0.TestRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.TestStatus_SUCCESS, test.Status.State)

}

func testRun(t *testing.T, runtime *Runtime, ctx context.Context, identity *basev0.ServiceIdentity, networkMappings []*basev0.NetworkMapping) {

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, networkMappings, runtime.GoGrpc.RestEndpoint, resources.NewNativeNetworkAccess())
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

func testConnectEndpoint(t *testing.T, runtime *Runtime, ctx context.Context, identity *basev0.ServiceIdentity, networkMappings []*basev0.NetworkMapping) {
	if runtime.GoGrpc.ConnectEndpoint == nil {
		t.Log("no connect endpoint configured, skipping connect test")
		return
	}

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, networkMappings, runtime.GoGrpc.ConnectEndpoint, resources.NewNativeNetworkAccess())
	require.NoError(t, err)

	// Connect protocol uses POST with JSON body to /api.WebService/Version
	client := http.Client{Timeout: 2 * time.Second}
	tries := 0
	for {
		if tries > 10 {
			t.Fatal("connect endpoint: too many tries")
		}
		time.Sleep(time.Second)

		req, err := http.NewRequest("POST", fmt.Sprintf("%s/api.WebService/Version", instance.Address), strings.NewReader("{}"))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		response, err := client.Do(req)
		if err != nil {
			tries++
			continue
		}
		if response.StatusCode != http.StatusOK {
			tries++
			response.Body.Close()
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
		t.Log("Connect endpoint working:", version)
		return
	}
}

func testNoApi(t *testing.T, runtime *Runtime, ctx context.Context, networkMappings []*basev0.NetworkMapping) {
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, networkMappings, runtime.GoGrpc.RestEndpoint, resources.NewNativeNetworkAccess())
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
