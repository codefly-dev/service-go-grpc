package main

import (
	"context"
	"encoding/json"
	"fmt"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/network"
	runners "github.com/codefly-dev/core/runners/base"
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

func TestSetRuntimeContextNixHintUsesLocalFirst(t *testing.T) {
	ctx := context.Background()

	runtime := NewRuntime(NewService())
	runtime.Base.Runtime.RuntimeContext = resources.NewRuntimeContextNative() // start with native

	err := runtime.SetRuntimeContext(ctx, resources.NewRuntimeContextNix())
	require.NoError(t, err)
	if languages.HasGoRuntime(nil) {
		require.Equal(t, resources.RuntimeContextNative, runtime.Base.Runtime.RuntimeContext.Kind)
		require.True(t, runtime.Base.Runtime.IsNativeRuntime())
	} else if runners.CheckNixInstalled() && runners.IsNixSupported() {
		require.Equal(t, resources.RuntimeContextNix, runtime.Base.Runtime.RuntimeContext.Kind)
		require.True(t, runtime.Base.Runtime.IsNixRuntime())
	} else {
		require.Equal(t, resources.RuntimeContextContainer, runtime.Base.Runtime.RuntimeContext.Kind)
		require.True(t, runtime.Base.Runtime.IsContainerRuntime())
	}
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
	// CONNECT support is now in the pinned core (the factory templates
	// reference standards.CONNECT directly), so the scaffolded service
	// resolves it — the old t.Skip is no longer warranted.
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

	creationMode := &builderv0.CreationMode{Communicate: withConnect}
	resp, err := builder.Load(ctx, &builderv0.LoadRequest{Identity: identity, CreationMode: creationMode})
	require.NoError(t, err)
	require.NotNil(t, resp)
	if withConnect {
		confirm := func(value bool) *agentv0.Answer {
			return &agentv0.Answer{Value: &agentv0.Answer_Confirm{Confirm: &agentv0.ConfirmAnswer{Confirmed: value}}}
		}
		builder.answers = map[string]*agentv0.Answer{
			HotReload:                 confirm(true),
			DebugSymbols:              confirm(false),
			RaceConditionDetectionRun: confirm(false),
			RestEndpointSetting:       confirm(true),
			ConnectEndpointSetting:    confirm(true),
		}
	}

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

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

	// Connect protocol uses POST with JSON body to the generated service path.
	client := http.Client{Timeout: 2 * time.Second}
	baseURL := instance.Address
	if !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	tries := 0
	for {
		if tries > 10 {
			t.Fatal("connect endpoint: too many tries")
		}
		time.Sleep(time.Second)

		procedure := fmt.Sprintf("/api.%sService/Version", shared.ToTitle(identity.Name))
		req, err := http.NewRequest("POST", baseURL+procedure, strings.NewReader("{}"))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connect-Protocol-Version", "1")

		response, err := client.Do(req)
		if err != nil {
			t.Logf("Connect request to %s failed: %v", req.URL, err)
			tries++
			continue
		}
		if response.StatusCode != http.StatusOK {
			body, readErr := io.ReadAll(response.Body)
			t.Logf("Connect endpoint returned %s: %s", response.Status, strings.TrimSpace(string(body)))
			if readErr != nil {
				t.Logf("reading Connect error response: %v", readErr)
			}
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
