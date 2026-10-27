package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/shared"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/templates"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/imports"
)

// TestConnectOnlyFactoryServesEveryProtocol renders the actual factory without
// REST or a gRPC endpoint, compiles its main, then runs the wire regression in
// that generated module. No Docker backend can silently skip this coverage.
func TestConnectOnlyFactoryServesEveryProtocol(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, filepath.WalkDir("base/code", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel("base/code", path)
		if err != nil {
			return err
		}
		target := filepath.Join(root, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if strings.HasSuffix(path, "_test.go") && entry.Name() != "connect_only_test.go" && entry.Name() != "main_test.go" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	}))
	config := baseCreateConfiguration(&Settings{ConnectEndpoint: true})
	config.ConnectOnly = true
	config.ConnectEndpointName = "face2"
	for _, name := range []string{"main.go", "pkg/adapters/grpc_gen.go", "pkg/adapters/server_gen.go", "pkg/adapters/connect_gen.go", "pkg/adapters/rest_gen.go", "pkg/adapters/cors_gen.go"} {
		raw, err := factoryFS.ReadFile("templates/factory/code/" + name + ".tmpl")
		require.NoError(t, err)
		rendered, err := templates.ApplyTemplate(string(raw), config)
		require.NoError(t, err)
		formatted, err := imports.Process(name, []byte(rendered), &imports.Options{Comments: true})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(root, name), formatted, 0644))
		if name == "main.go" {
			require.NotContains(t, string(formatted), "API(standards.GRPC)")
		}
	}
	command := exec.CommandContext(t.Context(), "go", "test", "-mod=readonly", "-count=1", "./...")
	command.Dir = root
	command.Env = append(os.Environ(), "GOWORK=off", "TEST_CONNECT_ONLY=1")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

// TestConnectOnlyDeployment advertises just the declared listener and puts all
// three Kubernetes probes on it; a successful build with dead probes is unusable.
func TestConnectOnlyDeployment(t *testing.T) {
	params := DeploymentParameters{ConnectOnly: true, ConnectPort: 18081, Health: HealthSpec{Mode: HealthModeGrpc}}
	container := renderContainer(t, params)
	require.Len(t, container.Ports, 1)
	require.EqualValues(t, 18081, container.Ports[0].ContainerPort)
	require.EqualValues(t, 18081, container.ReadinessProbe.GRPC.Port)
	require.EqualValues(t, 18081, container.StartupProbe.GRPC.Port)
	require.Equal(t, "connect", container.LivenessProbe.TCPSocket.Port.StrVal)
}

// TestFactoryMainMatchesBase locks endpoint discovery as well as the adapters.
func TestFactoryMainMatchesBase(t *testing.T) {
	data, err := os.ReadFile("base/code/main.go")
	require.NoError(t, err)
	require.Equal(t, string(data), string(renderFactoryGo(t, "templates/factory/code/main.go.tmpl", &Settings{RestEndpoint: true, ConnectEndpoint: true})))
}

// TestConnectOnlyLoadsAndExportsOneEndpoint exercises the refusal that prompted
// this change through real Builder and Runtime clients, then lets Core allocate
// and export the sole listener instead of inventing a gRPC carrier.
func TestConnectOnlyLoadsAndExportsOneEndpoint(t *testing.T) {
	for _, withGRPC := range []bool{false, true} {
		t.Run(fmt.Sprintf("grpc=%v", withGRPC), func(t *testing.T) {
			ctx := t.Context()
			identity := goGrpcServiceFixture(t)
			dir := filepath.Join(identity.WorkspacePath, identity.RelativeToWorkspace)
			service, err := resources.LoadServiceFromDir(ctx, dir)
			require.NoError(t, err)
			service.Endpoints = []*resources.Endpoint{{Name: "connect", API: "connect"}}
			if withGRPC {
				service.Endpoints = append(service.Endpoints, &resources.Endpoint{Name: "grpc", API: "grpc"})
				data, err := os.ReadFile("base/proto/api.proto")
				require.NoError(t, err)
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "proto"), 0755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "proto/api.proto"), data, 0644))
			}
			service.Spec = map[string]any{"connect-endpoint": true, "rest-endpoint": false, "hot-reload": false}
			require.NoError(t, service.SaveAtDir(ctx, dir))
			builderClient, builder := startBuilderAgent(t, identity)
			loaded, err := builderClient.Load(ctx, &builderv0.LoadRequest{Identity: identity})
			require.NoError(t, err)
			require.NotEqual(t, builderv0.LoadStatus_ERROR, loaded.GetState().GetState(), loaded)
			require.NotNil(t, builder.GoGrpc.ConnectEndpoint)
			require.Equal(t, withGRPC, builder.GoGrpc.GrpcEndpoint != nil)

			runtime := NewRuntime(NewService())
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			server := grpc.NewServer()
			runtimev0.RegisterRuntimeServer(server, runtime)
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(server.Stop)
			conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			client := runtimev0.NewRuntimeClient(conn)
			env := resources.LocalEnvironment()
			load, err := client.Load(ctx, &runtimev0.LoadRequest{Identity: identity, Environment: shared.Must(env.Proto()), DisableCatch: true})
			require.NoError(t, err)
			require.Equal(t, runtimev0.LoadStatus_READY, load.GetStatus().GetState(), load.GetStatus())
			require.Len(t, load.Endpoints, len(service.Endpoints))
			manager, err := network.NewRuntimeManager(ctx, nil)
			require.NoError(t, err)
			manager.WithTemporaryPorts()
			mappings, err := manager.GenerateNetworkMappings(ctx, env, &resources.Workspace{Name: "test"}, runtime.Identity, load.Endpoints, resources.NewRuntimeContextNative())
			require.NoError(t, err)
			t.Cleanup(func() { _, _ = client.Destroy(context.Background(), &runtimev0.DestroyRequest{}) })
			init, err := client.Init(ctx, &runtimev0.InitRequest{RuntimeContext: resources.NewRuntimeContextNative(), ProposedNetworkMappings: mappings})
			require.NoError(t, err)
			require.Equal(t, runtimev0.InitStatus_READY, init.GetStatus().GetState(), init.GetStatus())
			require.Len(t, runtime.EnvironmentVariables.Endpoints(), len(service.Endpoints))
			if !withGRPC {
				require.Equal(t, "connect", runtime.GoGrpc.rpcEndpoint().Api)
			}
		})
	}
}

// TestMissingRPCListenerNamesTheReason keeps omission distinct from endpoint
// ambiguity: accepting Connect must not silently accept a service with no RPCs.
func TestMissingRPCListenerNamesTheReason(t *testing.T) {
	identity := goGrpcServiceFixture(t)
	client, _ := startBuilderAgent(t, identity)
	response, err := client.Load(t.Context(), &builderv0.LoadRequest{Identity: identity})
	require.NoError(t, err)
	require.Equal(t, builderv0.LoadStatus_ERROR, response.GetState().GetState())
	require.Contains(t, response.GetState().GetMessage(), "no RPC listener")
}

// TestMarkedEntrypointCanRefreshWithoutOwningCustomAdapters prevents a custom
// protobuf service name from freezing generated endpoint discovery forever.
func TestMarkedEntrypointCanRefreshWithoutOwningCustomAdapters(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "code/main.go"), "// This code is generated by the agent\npackage main\n")
	writeTestFile(t, filepath.Join(root, "proto/api.proto"), "syntax = \"proto3\"; service CustomService {}")
	targets, _, err := generatedScaffoldTargets(root, filepath.Join(root, "proto"), "WebService")
	require.NoError(t, err)
	require.Empty(t, targets)
	targets, _, err = generatedEntrypointTarget(root)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join("code", "main.go")}, targets)
	selection := generatedScaffoldSelection{mainOnly: true}
	require.True(t, selection.Keep("code"))
	require.True(t, selection.Keep("main.go.tmpl"))
	for _, name := range []string{"pkg", "adapters", "plugins", "grpc_gen.go.tmpl", "server_gen.go.tmpl", "rpcs.go.tmpl"} {
		require.False(t, selection.Keep(name), name)
	}
	writeTestFile(t, filepath.Join(root, "code/main.go"), "package main\n")
	targets, reason, err := generatedEntrypointTarget(root)
	require.NoError(t, err)
	require.Empty(t, targets)
	require.Contains(t, reason, "hand-written")
}

// Custom services such as runtime/tasks disable REST scaffolding but declare a
// REST endpoint for hand-owned adapters. Refreshing their marked main must keep
// the port lookup that enables those adapters, including across repeated syncs.
func TestCustomEntrypointSyncPreservesDeclaredListeners(t *testing.T) {
	protoCompanionMu.Lock()
	defer protoCompanionMu.Unlock()
	for _, tc := range []struct {
		name, endpoint, api, port string
		grpc, connect             bool
	}{
		{"tasks REST", "rest", "rest", "EndpointHttpPort", true, false},
		{"custom Connect", "connect", "connect", "EndpointConnectPort", true, false},
		{"Connect only", "connect", "connect", "EndpointConnectPort", false, true},
		// A sole Connect endpoint keeping its gRPC-era name: main must ask for
		// it by that name, or the carrier keyed by it is never found.
		{"Connect only named grpc", "grpc", "connect", "EndpointConnectPort", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			identity := goGrpcServiceFixture(t)
			dir := filepath.Join(identity.WorkspacePath, identity.RelativeToWorkspace)
			service, err := resources.LoadServiceFromDir(ctx, dir)
			require.NoError(t, err)
			service.Name, identity.Name = "tasks", "tasks"
			service.Endpoints = []*resources.Endpoint{{Name: tc.endpoint, API: tc.api}}
			if tc.grpc {
				service.Endpoints = append(service.Endpoints, &resources.Endpoint{Name: "grpc", API: "grpc"})
			}
			service.Spec = map[string]any{"rest-endpoint": false, "connect-endpoint": tc.connect}
			require.NoError(t, service.SaveAtDir(ctx, dir))
			writeTestFile(t, filepath.Join(dir, "code/main.go"), "// This code is generated by the agent\npackage main\n")
			writeTestFile(t, filepath.Join(dir, "proto/api.proto"), `syntax = "proto3";
package tasks;
option go_package = "example.com/svc/pkg/gen;gen";
service TaskService {}
`)
			writeTestFile(t, filepath.Join(dir, "proto/buf.yaml"), "version: v1\n")
			writeTestFile(t, filepath.Join(dir, "proto/buf.gen.yaml"), "version: v1\nplugins:\n  - plugin: go\n    out: ../code/pkg/gen\n    opt: paths=source_relative\n")
			adapter := "package adapters\n\nfunc (s *Server) Start() error {\n\tif s.config." + tc.port + " == nil {\n\t\treturn s.server.Serve(s.listener)\n\t}\n\treturn s.serveHTTP()\n}\n"
			writeTestFile(t, filepath.Join(dir, "code/pkg/adapters/server.go"), adapter)
			client, builder := startBuilderAgent(t, identity)
			loaded, err := client.Load(ctx, &builderv0.LoadRequest{Identity: identity})
			require.NoError(t, err)
			require.NotEqual(t, builderv0.LoadStatus_ERROR, loaded.GetState().GetState(), loaded)
			response, err := client.Sync(ctx, &builderv0.SyncRequest{})
			require.NoError(t, err)
			require.NoError(t, syncResponseError(response))
			main, err := os.ReadFile(filepath.Join(dir, "code/main.go"))
			require.NoError(t, err)
			query := "codefly.For(ctx).API(standards." + strings.ToUpper(tc.api) + ")"
			if tc.endpoint != tc.api {
				query = "codefly.For(ctx).Endpoint(\"" + tc.endpoint + "\").API(standards." + strings.ToUpper(tc.api) + ")"
			}
			require.Contains(t, string(main), query)
			require.Contains(t, string(main), "config."+tc.port+" = shared.Pointer(")
			require.NotContains(t, string(main), "Host = listenerHost", "custom adapters do not acquire new Configuration fields through main-only Sync")
			require.Equal(t, tc.grpc, strings.Contains(string(main), "API(standards.GRPC)"))
			assertTestFile(t, filepath.Join(dir, "code/pkg/adapters/server.go"), adapter)
			require.NoFileExists(t, filepath.Join(dir, "code/pkg/adapters/server_gen.go"))
			require.False(t, builder.GoGrpc.Settings.RestEndpoint, "rendering must not enable scaffold ownership")
			require.Equal(t, tc.connect, builder.GoGrpc.Settings.ConnectEndpoint)
			response, err = client.Sync(ctx, &builderv0.SyncRequest{DryRun: true})
			require.NoError(t, err)
			require.NoError(t, syncResponseError(response))
			require.Empty(t, response.GetChangedFiles())
		})
	}
}

// A main-only sync cannot extend a service-owned Configuration. Compile the
// actual template against the established seam, so new field assignments fail
// here rather than breaking a custom service on its next sync.
func TestCustomEntrypointBuildsAgainstPortOnlyAdapters(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile(filepath.Join("base/code", name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(root, name), data, 0644))
	}
	config := baseCreateConfiguration(&Settings{RestEndpoint: true, ConnectEndpoint: true})
	config.CustomAdapters = true
	raw, err := factoryFS.ReadFile("templates/factory/code/main.go.tmpl")
	require.NoError(t, err)
	rendered, err := templates.ApplyTemplate(string(raw), config)
	require.NoError(t, err)
	formatted, err := imports.Process("main.go", []byte(rendered), &imports.Options{Comments: true})
	require.NoError(t, err)
	writeTestFile(t, filepath.Join(root, "main.go"), string(formatted))
	// Compile-only contract fixture; no calls or listener behavior are simulated.
	writeTestFile(t, filepath.Join(root, "pkg/adapters/server.go"), `package adapters
import "context"
type Configuration struct {
 EndpointGrpcPort uint16
 EndpointHttpPort *uint16
 EndpointConnectPort *uint16
}
type Server struct{}
func NewServer(*Configuration) (*Server, error) { panic("compile-only fixture") }
func (*Server) Start(context.Context) error { panic("compile-only fixture") }
func (*Server) Stop() { panic("compile-only fixture") }
`)
	command := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "./...")
	command.Dir = root
	command.Env = append(os.Environ(), "GOWORK=off")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

// TestConnectOnlyEntrypointStartsOnAListenerNotNamedConnect builds the
// generated entrypoint of a Connect-only service whose sole endpoint keeps
// another name (`grpc`, as a service migrated from a gRPC listener does) and
// starts it with exactly the environment the Runtime injects. Core allows the
// name; the composition keys the carrier by name and API. A main that asked by
// API alone was looked up as `connect`, found no carrier and panicked at start,
// which no render-only or adapter-level test can see.
func TestConnectOnlyEntrypointStartsOnAListenerNotNamedConnect(t *testing.T) {
	ctx := t.Context()
	identity := goGrpcServiceFixture(t)
	dir := filepath.Join(identity.WorkspacePath, identity.RelativeToWorkspace)
	service, err := resources.LoadServiceFromDir(ctx, dir)
	require.NoError(t, err)
	service.Endpoints = []*resources.Endpoint{{Name: "grpc", API: "connect"}}
	service.Spec = map[string]any{"connect-endpoint": true, "rest-endpoint": false, "hot-reload": false}
	require.NoError(t, service.SaveAtDir(ctx, dir))
	builderClient, builder := startBuilderAgent(t, identity)
	loaded, err := builderClient.Load(ctx, &builderv0.LoadRequest{Identity: identity})
	require.NoError(t, err)
	require.NotEqual(t, builderv0.LoadStatus_ERROR, loaded.GetState().GetState(), loaded)
	require.Nil(t, builder.GoGrpc.GrpcEndpoint)

	// The generated module: base/code with the factory rendered as Sync
	// renders it for this declaration.
	root := t.TempDir()
	require.NoError(t, os.CopyFS(root, os.DirFS("base/code")))
	config := baseCreateConfiguration(builder.GoGrpc.Settings)
	config.ConnectOnly = true
	require.NoError(t, config.nameDeclaredListeners(ctx, builder.Endpoints))
	for _, name := range []string{"main.go", "pkg/adapters/grpc_gen.go", "pkg/adapters/server_gen.go", "pkg/adapters/connect_gen.go", "pkg/adapters/rest_gen.go", "pkg/adapters/cors_gen.go"} {
		raw, err := factoryFS.ReadFile("templates/factory/code/" + name + ".tmpl")
		require.NoError(t, err)
		rendered, err := templates.ApplyTemplate(string(raw), config)
		require.NoError(t, err)
		formatted, err := imports.Process(name, []byte(rendered), &imports.Options{Comments: true})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(root, name), formatted, 0644))
	}
	binary := filepath.Join(t.TempDir(), "service")
	build := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-o", binary, ".")
	build.Dir = root
	build.Env = append(os.Environ(), "GOWORK=off")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "%s", output)

	// The Runtime's own allocation and injection for this declaration.
	runtime := NewRuntime(NewService())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	runtimev0.RegisterRuntimeServer(server, runtime)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := runtimev0.NewRuntimeClient(conn)
	env := resources.LocalEnvironment()
	load, err := client.Load(ctx, &runtimev0.LoadRequest{Identity: identity, Environment: shared.Must(env.Proto()), DisableCatch: true})
	require.NoError(t, err)
	require.Equal(t, runtimev0.LoadStatus_READY, load.GetStatus().GetState(), load.GetStatus())
	manager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	manager.WithTemporaryPorts()
	mappings, err := manager.GenerateNetworkMappings(ctx, env, &resources.Workspace{Name: "test"}, runtime.Identity, load.Endpoints, resources.NewRuntimeContextNative())
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = client.Destroy(context.Background(), &runtimev0.DestroyRequest{}) })
	init, err := client.Init(ctx, &runtimev0.InitRequest{RuntimeContext: resources.NewRuntimeContextNative(), ProposedNetworkMappings: mappings})
	require.NoError(t, err)
	require.Equal(t, runtimev0.InitStatus_READY, init.GetStatus().GetState(), init.GetStatus())
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, mappings, runtime.GoGrpc.rpcEndpoint(), resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	injected, err := runtime.EnvironmentVariables.All()
	require.NoError(t, err)

	var logs strings.Builder
	start := exec.CommandContext(ctx, binary)
	start.Dir = t.TempDir()
	start.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, resources.EnvironmentVariableAsStrings(injected)...)
	start.Stdout, start.Stderr = &logs, &logs
	require.NoError(t, start.Start())
	exited := make(chan struct{})
	var exit error
	go func() { exit = start.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = start.Process.Kill()
		<-exited
	})

	address := fmt.Sprintf("127.0.0.1:%d", instance.Port)
	health, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = health.Close() })
	require.Eventually(t, func() bool {
		select {
		case <-exited:
			return true
		default:
		}
		response, err := grpc_health_v1.NewHealthClient(health).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
		return err == nil && response.GetStatus() == grpc_health_v1.HealthCheckResponse_SERVING
	}, 30*time.Second, 100*time.Millisecond)
	select {
	case <-exited:
		t.Fatalf("the generated service exited (%v):\n%s", exit, logs.String())
	default:
	}
	// The same listener answers the service's own API over Connect.
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+"/api.WebService/Version", strings.NewReader("{}"))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connect-Protocol-Version", "1")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, "%s", body)
	require.Contains(t, string(body), `"version"`)
}
