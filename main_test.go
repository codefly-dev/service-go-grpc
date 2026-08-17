package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/codefly-dev/core/companions/proto"
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
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"

	"github.com/codefly-dev/core/resources"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
)

func TestProtoImageSupportsHost(t *testing.T) {
	hostList := fmt.Sprintf(
		`{"manifests":[{"platform":{"os":"linux","architecture":%q}},{"platform":{"os":"unknown","architecture":"unknown"}}]}`,
		runtime.GOARCH)
	// The amd64-only proto manifest as seen from a non-amd64 host.
	foreignArch := "amd64"
	if runtime.GOARCH == "amd64" {
		foreignArch = "arm64"
	}
	foreignList := fmt.Sprintf(
		`{"manifests":[{"platform":{"os":"linux","architecture":%q}},{"platform":{"os":"unknown","architecture":"unknown"}}]}`,
		foreignArch)

	tests := []struct {
		name             string
		manifest         string
		wantList         bool
		wantSupportsHost bool
	}{
		{"host arch present", hostList, true, true},
		{"only foreign arch", foreignList, true, false},
		{"non-linux host arch ignored", fmt.Sprintf(`{"manifests":[{"platform":{"os":"windows","architecture":%q}}]}`, runtime.GOARCH), true, false},
		{"attestation only", `{"manifests":[{"platform":{"os":"unknown","architecture":"unknown"}}]}`, false, false},
		{"single arch image", `{"schemaVersion":2,"config":{}}`, false, false},
		{"unparseable", `not json`, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isList, supportsHost := protoImageSupportsHost([]byte(tc.manifest))
			require.Equal(t, tc.wantList, isList)
			require.Equal(t, tc.wantSupportsHost, supportsHost)

			// requireProtoCompanionImage fails the test loudly when, and only
			// when, this predicate holds. Locking it here guards the fail-loud
			// contract: only a manifest list that positively lacks the host arch
			// (the amd64-only image on arm64) may block; a host-supporting list
			// (linux/amd64 on CI) and every ambiguous shape must not.
			blocked := isList && !supportsHost
			require.Equal(t, tc.wantList && !tc.wantSupportsHost, blocked)
		})
	}
}

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

// requireProtoCompanionImage fails a proto-generating integration test when the
// pinned proto companion image cannot be obtained for the host. The image is a
// manifest list that currently ships linux/amd64 only, so pulling it on Apple
// Silicon fails with "no matching manifest for linux/arm64".
//
// We fail loudly rather than skipping, matching core's testutil.RequireProtoImage:
// a silently-skipped integration test makes `go test ./...` report ok while the
// create-to-run path never ran, masking regressions and letting environmental
// drift hide bugs the test exists to catch. The remedy is in the message — build
// the image locally for the host arch.
//
// Nix could in principle supply the companion on Apple Silicon, but core's
// runners/companion.detectBackend selects Docker unconditionally whenever the
// engine is running and never falls back, so an absent host-arch image genuinely
// blocks proto generation. The failure therefore fires exactly when the real run
// would fail: Docker is up but cannot get the image for this arch. Any ambiguity
// (image already built locally, Docker/manifest unreadable — e.g. a Docker-less
// Nix host, where detectBackend uses Nix and the run succeeds) proceeds so the
// genuine outcome surfaces on its own terms.
func requireProtoCompanionImage(t *testing.T, ctx context.Context) {
	t.Helper()

	image, err := proto.CompanionImage(ctx)
	require.NoError(t, err)
	ref := image.Name + ":" + image.Tag

	if exec.CommandContext(ctx, "docker", "image", "inspect", ref).Run() == nil {
		return
	}

	manifest, err := exec.CommandContext(ctx, "docker", "manifest", "inspect", ref).Output()
	if err != nil {
		return
	}
	if isList, supportsHost := protoImageSupportsHost(manifest); isList && !supportsHost {
		t.Fatalf("proto companion image %s has no linux/%s manifest; run `codefly companion build --all` to build it locally",
			ref, runtime.GOARCH)
	}
}

// protoImageSupportsHost reports whether a `docker manifest inspect` payload is a
// multi-arch manifest list (isList) and, if so, whether it carries a linux entry
// for the host architecture (supportsHost). Companion images are linux, and a
// default `docker pull` of a manifest list will not fall back to a non-host
// architecture — it fails outright when the host arch is absent, rather than
// emulating another arch — so the run needs a linux/runtime.GOARCH entry
// regardless of runtime.GOOS. Attestation entries (os/arch "unknown") are ignored
// so they count as neither a real platform nor host support.
func protoImageSupportsHost(manifest []byte) (isList bool, supportsHost bool) {
	var list struct {
		Manifests []struct {
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if json.Unmarshal(manifest, &list) != nil {
		return false, false
	}
	for _, m := range list.Manifests {
		if m.Platform.OS == "unknown" || m.Platform.Architecture == "unknown" {
			continue
		}
		isList = true
		if m.Platform.OS == "linux" && m.Platform.Architecture == runtime.GOARCH {
			supportsHost = true
		}
	}
	return isList, supportsHost
}

func testCreateToRun(t *testing.T, runtimeContext *basev0.RuntimeContext, withConnect bool) {
	wool.SetGlobalLogLevel(wool.DEBUG)
	ctx := context.Background()

	requireProtoCompanionImage(t, ctx)

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
