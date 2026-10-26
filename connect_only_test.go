package main

import (
	"context"
	"fmt"
	"io/fs"
	"net"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/shared"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
		if strings.HasSuffix(path, "_test.go") && entry.Name() != "connect_only_test.go" {
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
	command.Env = append(os.Environ(), "GOWORK=off")
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
