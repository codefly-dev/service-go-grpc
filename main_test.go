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
	"sync"
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

// TestSettingsValidateMcpWithoutRest proves MCP no longer rides the REST
// listener: mcp-endpoint is valid on its own (it has a dedicated endpoint), and
// the allowlist is still validated.
func TestSettingsValidateMcpWithoutRest(t *testing.T) {
	valid := &Settings{McpEndpoint: true, McpMethods: []string{"api.WebService/Version"}}
	require.NoError(t, valid.Validate())

	badMethods := &Settings{McpEndpoint: true, McpMethods: []string{"no-slash"}}
	require.Error(t, badMethods.Validate())
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

// protoCompanionMu serializes the proto-generation phase of the parallel
// create-to-run cases: the shared proto companion names its container purely
// from a millisecond timestamp, so concurrent generation risks a container
// name conflict. See its use in testCreateToRun.
var protoCompanionMu sync.Mutex

// These create-to-run cases each drive the full pipeline — proto companion
// codegen, a Go/Docker build of the scaffolded service, then a live boot —
// so a single case runs on the order of a minute. Run serially they overrun
// the release packaging stage's build timeout; t.Parallel lets the three
// overlap on their independent temp dirs and temporary ports so the suite's
// wall-clock is the slowest case, not their sum. The one resource they do
// share — the proto companion container — is guarded by protoCompanionMu.
func TestCreateToRunNative(t *testing.T) {
	t.Parallel()
	if languages.HasGoRuntime(nil) {
		testCreateToRun(t, resources.NewRuntimeContextNative(), false)
	}
}

func TestCreateToRunDocker(t *testing.T) {
	t.Parallel()
	testCreateToRun(t, resources.NewRuntimeContextContainer(), false)
}

func TestCreateToRunWithConnectNative(t *testing.T) {
	t.Parallel()
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
			McpEndpointSetting:        confirm(true),
		}
		// The allowlist is a YAML-only field (no confirm question), so set it
		// directly. This makes the generated service resolve a real MCP tool at
		// startup, exercising the registry lookup in the built binary. The
		// generated proto service is api.<Title>Service (see api.proto.tmpl).
		builder.GoGrpc.Settings.McpMethods = []string{fmt.Sprintf("api.%sService/Version", shared.ToTitle(identity.Name))}
	}

	// Proto generation runs the shared proto companion, whose Docker container
	// is named proto-<unix-milli> with no per-service component (core
	// companions/proto). Two of these parallel cases generating in the same
	// millisecond would request the same container name and one would fail with
	// a name conflict, so serialize just the generation phase; the dominant
	// build-and-run phase below still overlaps across cases.
	func() {
		protoCompanionMu.Lock()
		defer protoCompanionMu.Unlock()

		createResponse, err := builder.Create(ctx, &builderv0.CreateRequest{})
		require.NoError(t, err)
		require.Equal(
			t,
			builderv0.CreateStatus_CREATED,
			createResponse.GetState().GetState(),
			createResponse.GetState().GetMessage(),
		)
		syncResponse, err := builder.Sync(ctx, &builderv0.SyncRequest{DryRun: true})
		require.NoError(t, err)
		require.Equal(t, builderv0.SyncStatus_SUCCESS, syncResponse.GetState().GetState(), syncResponse.GetState().GetMessage())
		require.Empty(t, syncResponse.GetChangedFiles(), "a newly created service must already be sync-clean")
	}()

	// Now run it
	runtime := NewRuntime(NewService())

	_, err = runtime.Load(ctx, &runtimev0.LoadRequest{
		Identity:     identity,
		Environment:  shared.Must(env.Proto()),
		DisableCatch: true})
	require.NoError(t, err)

	expectedEndpoints := 2 // grpc + rest
	if withConnect {
		expectedEndpoints = 4 // grpc + rest + connect + mcp
	}
	require.Equal(t, expectedEndpoints, len(runtime.Endpoints))

	// Create temporary network mappings
	networkManager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	networkManager.WithTemporaryPorts()

	networkMappings, err := networkManager.GenerateNetworkMappings(ctx, env, workspace, runtime.Identity, runtime.Endpoints, runtimeContext)
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

	// Test MCP endpoint (if configured)
	testMcpEndpoint(t, runtime, ctx, identity, networkMappings)

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

	const readinessTimeout = 30 * time.Second
	const readinessPollInterval = 200 * time.Millisecond
	client := http.Client{Timeout: time.Second}
	deadline := time.Now().Add(readinessTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		response, err := client.Get(fmt.Sprintf("%s/version", instance.Address))
		if err != nil {
			lastErr = err
			time.Sleep(readinessPollInterval)
			continue
		}
		if response.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("version endpoint returned %s", response.Status)
			response.Body.Close()
			time.Sleep(readinessPollInterval)
			continue
		}

		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		require.NoError(t, err)

		var data map[string]interface{}
		err = json.Unmarshal(body, &data)
		require.NoError(t, err)

		version, ok := data["version"].(string)
		require.True(t, ok)
		require.Equal(t, identity.Version, version)

		// The gateway proxies grpc.health.v1 as /healthz
		healthResponse, err := client.Get(fmt.Sprintf("%s/healthz", instance.Address))
		if err != nil {
			lastErr = err
			time.Sleep(readinessPollInterval)
			continue
		}
		healthResponse.Body.Close()
		if healthResponse.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("health endpoint returned %s", healthResponse.Status)
			time.Sleep(readinessPollInterval)
			continue
		}
		return
	}
	t.Fatalf("REST endpoint %s was not ready within %s: %v", instance.Address, readinessTimeout, lastErr)
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
	const readinessTimeout = 30 * time.Second
	const readinessPollInterval = 200 * time.Millisecond
	deadline := time.Now().Add(readinessTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		procedure := fmt.Sprintf("/api.%sService/Version", shared.ToTitle(identity.Name))
		req, err := http.NewRequest("POST", baseURL+procedure, strings.NewReader("{}"))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connect-Protocol-Version", "1")

		response, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(readinessPollInterval)
			continue
		}
		if response.StatusCode != http.StatusOK {
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil {
				lastErr = fmt.Errorf("Connect endpoint returned %s and its body could not be read: %w", response.Status, readErr)
			} else {
				lastErr = fmt.Errorf("Connect endpoint returned %s: %s", response.Status, strings.TrimSpace(string(body)))
			}
			time.Sleep(readinessPollInterval)
			continue
		}

		body, err := io.ReadAll(response.Body)
		response.Body.Close()
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
	t.Fatalf("Connect endpoint %s was not ready within %s: %v", baseURL, readinessTimeout, lastErr)
}

func testMcpEndpoint(t *testing.T, runtime *Runtime, ctx context.Context, identity *basev0.ServiceIdentity, networkMappings []*basev0.NetworkMapping) {
	if runtime.GoGrpc.McpEndpoint == nil {
		t.Log("no mcp endpoint configured, skipping mcp test")
		return
	}

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, networkMappings, runtime.GoGrpc.McpEndpoint, resources.NewNativeNetworkAccess())
	require.NoError(t, err)

	baseURL := instance.Address
	if !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}

	client := http.Client{Timeout: 2 * time.Second}
	const readinessTimeout = 30 * time.Second
	const readinessPollInterval = 200 * time.Millisecond
	deadline := time.Now().Add(readinessTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		// MCP rides its own Streamable HTTP listener at /mcp. A same-origin
		// initialize proves the dedicated port is bound and serving MCP.
		req, err := http.NewRequest(http.MethodPost, baseURL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")

		response, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(readinessPollInterval)
			continue
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("MCP endpoint returned %s", response.Status)
			time.Sleep(readinessPollInterval)
			continue
		}

		// A non-/mcp path on the dedicated listener is not served.
		other, err := client.Get(baseURL + "/version")
		require.NoError(t, err)
		other.Body.Close()
		require.Equal(t, http.StatusNotFound, other.StatusCode)

		t.Log("MCP endpoint working on", instance.Address)
		return
	}
	t.Fatalf("MCP endpoint %s was not ready within %s: %v", baseURL, readinessTimeout, lastErr)
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
