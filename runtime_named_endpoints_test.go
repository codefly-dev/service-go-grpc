package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
	"github.com/stretchr/testify/require"
)

// namedEndpointName is the endpoint of the shape that exposed the defect: one
// API served on a second, named endpoint beside the conventional ones.
const namedEndpointName = "authority"

// loadNamedEndpointRuntime loads a real service directory declaring grpc, rest
// and a named grpc endpoint, and proposes network mappings for every endpoint
// Load reported — exactly as the CLI's runner does (GenerateNetworkMappings over
// LoadResponse.Endpoints).
func loadNamedEndpointRuntime(t *testing.T, runtimeContext *basev0.RuntimeContext) (*Runtime, []*basev0.NetworkMapping, string) {
	t.Helper()
	ctx := context.Background()

	workspacePath := t.TempDir()
	relative := "mod/svc"
	serviceDir := filepath.Join(workspacePath, relative)
	require.NoError(t, os.MkdirAll(filepath.Join(serviceDir, "code"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(serviceDir, "code", "go.mod"), []byte("module testsvc\n\ngo 1.27\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(serviceDir, "code", "main.go"), []byte(cleanExitSource), 0o600))
	for _, contract := range []string{standards.ProtoPath, standards.OpenAPIPath} {
		data, err := os.ReadFile(filepath.Join("base", contract))
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(serviceDir, contract)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(serviceDir, contract), data, 0o600))
	}

	service := &resources.Service{
		Name:    "svc",
		Version: "0.0.0",
		Endpoints: []*resources.Endpoint{
			{Name: standards.GRPC, API: standards.GRPC},
			{Name: standards.REST, API: standards.REST},
			// core v0.15.0 retired `module` visibility: reach is private, internal or
			// public, and `internal` is what this fixture meant — reachable by
			// whatever composes the module.
			{Name: namedEndpointName, API: standards.GRPC, Visibility: resources.VisibilityInternal},
		},
		Spec: map[string]any{"rest-endpoint": true, "hot-reload": false},
	}
	require.NoError(t, service.SaveAtDir(ctx, serviceDir))
	require.NoError(t, (&resources.Module{Name: "mod"}).SaveToDir(ctx, filepath.Join(workspacePath, "mod")))

	workspace := &resources.Workspace{Name: "test"}
	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Version:             service.Version,
		Module:              "mod",
		Workspace:           workspace.Name,
		WorkspacePath:       workspacePath,
		RelativeToWorkspace: relative,
	}
	env := resources.LocalEnvironment()

	runtime := NewRuntime(NewService())
	load, err := runtime.Load(ctx, &runtimev0.LoadRequest{
		Identity:     identity,
		Environment:  shared.Must(env.Proto()),
		DisableCatch: true,
	})
	require.NoError(t, err)
	require.Equal(t, runtimev0.LoadStatus_READY, load.GetStatus().GetState(), load.GetStatus().GetMessage())
	require.Len(t, load.GetEndpoints(), 3, "Load must report the named endpoint, or the CLI never proposes a mapping for it")

	manager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	manager.WithTemporaryPorts()
	mappings, err := manager.GenerateNetworkMappings(ctx, env, workspace, runtime.Identity, load.GetEndpoints(), runtimeContext)
	require.NoError(t, err)
	require.Len(t, mappings, 3)

	return runtime, mappings, workspacePath
}

func namedEndpoint(t *testing.T, runtime *Runtime) *basev0.Endpoint {
	t.Helper()
	for _, endpoint := range runtime.Endpoints {
		if endpoint.Name == namedEndpointName {
			return endpoint
		}
	}
	t.Fatalf("service has no %q endpoint", namedEndpointName)
	return nil
}

// TestInitExportsNamedEndpoints holds that Init hands the service the address
// of every endpoint it declares, not only the conventional grpc/rest/connect
// ones. It is possible to get wrong because Init exports endpoints one setting
// at a time: a second endpoint serving the same API is proposed a mapping by the
// CLI and then silently dropped, so the service resolves "no network instance"
// for it at startup and never listens there. Both runtime contexts export the
// same environment, so both are held.
func TestInitExportsNamedEndpoints(t *testing.T) {
	if !languages.HasGoRuntime(nil) {
		t.Skip("native go toolchain not available")
	}
	for name, runtimeContext := range map[string]*basev0.RuntimeContext{
		"native":    resources.NewRuntimeContextNative(),
		"container": resources.NewRuntimeContextContainer(),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			runtime, mappings, workspacePath := loadNamedEndpointRuntime(t, runtimeContext)

			// A real native runner over the throwaway module, so Init's own runner
			// setup is exercised without building a container; the environment
			// Init exports does not depend on which runner carries it.
			runner, err := golanghelpers.NewNativeGoRunner(ctx, workspacePath, "mod/svc/code")
			require.NoError(t, err)
			runner.WithLocalCacheDir(filepath.Join(workspacePath, ".cache"))
			runtime.bindRunnerEnvironment(runner)

			init, err := runtime.Init(ctx, &runtimev0.InitRequest{
				RuntimeContext:          runtimeContext,
				ProposedNetworkMappings: mappings,
			})
			require.NoError(t, err)
			require.Equal(t, runtimev0.InitStatus_READY, init.GetStatus().GetState(), init.GetStatus().GetMessage())

			named := namedEndpoint(t, runtime)
			want, err := resources.FindNetworkInstanceInNetworkMappings(ctx, mappings, named, resources.NewNativeNetworkAccess())
			require.NoError(t, err)

			var exported []string
			found := false
			for _, access := range runtime.EnvironmentVariables.Endpoints() {
				exported = append(exported, resources.EndpointDestination(access.Endpoint))
				if resources.EndpointDestination(access.Endpoint) == resources.EndpointDestination(named) {
					require.Equal(t, want.GetAddress(), access.NetworkInstance.GetAddress())
					found = true
				}
			}
			require.Truef(t, found, "named endpoint %q is not in the service's environment; exported: %v", namedEndpointName, exported)
			// The conventional endpoints are still exported exactly once each.
			require.Len(t, exported, 3, "exported: %v", exported)

			// What the process actually receives: the carrier the SDK reads for
			// Endpoint("authority").API("grpc").
			all, err := runtime.EnvironmentVariables.All()
			require.NoError(t, err)
			carrier := resources.Env("CODEFLY__ENDPOINT__MOD__SVC__AUTHORITY__GRPC", want.GetAddress())
			require.Contains(t, resources.EnvironmentVariableAsStrings(all), resources.EnvironmentVariableAsStrings([]*resources.EnvironmentVariable{carrier})[0])
		})
	}
}

// TestInitRefusesADeclaredEndpointWithoutAMapping holds that an endpoint the
// service declares but was proposed no address for fails Init by name. The CLI
// proposes one mapping per endpoint Load reports, so a missing one is a broken
// contract; accepting it would boot a service that silently lacks a listener.
func TestInitRefusesADeclaredEndpointWithoutAMapping(t *testing.T) {
	if !languages.HasGoRuntime(nil) {
		t.Skip("native go toolchain not available")
	}
	ctx := context.Background()
	runtimeContext := resources.NewRuntimeContextNative()
	runtime, mappings, workspacePath := loadNamedEndpointRuntime(t, runtimeContext)

	runner, err := golanghelpers.NewNativeGoRunner(ctx, workspacePath, "mod/svc/code")
	require.NoError(t, err)
	runtime.bindRunnerEnvironment(runner)

	var withoutNamed []*basev0.NetworkMapping
	for _, mapping := range mappings {
		if mapping.GetEndpoint().GetName() != namedEndpointName {
			withoutNamed = append(withoutNamed, mapping)
		}
	}
	require.Len(t, withoutNamed, 2)

	init, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          runtimeContext,
		ProposedNetworkMappings: withoutNamed,
	})
	require.NoError(t, err)
	require.Equal(t, runtimev0.InitStatus_ERROR, init.GetStatus().GetState())
	require.Contains(t, init.GetStatus().GetMessage(), namedEndpointName)
}

// TestContainerRunnerPublishesNamedEndpointPorts holds that the container
// runtime publishes a named endpoint's port beside the conventional ones. The
// process inside the container binds the port it was handed; a port the
// container does not publish is unreachable from the host even though the
// service is listening, which reads as a healthy service nobody can call.
func TestContainerRunnerPublishesNamedEndpointPorts(t *testing.T) {
	ctx := context.Background()
	if !dockerrun.DockerEngineRunning(ctx) {
		t.Skip("docker engine not running")
	}
	runtimeContext := resources.NewRuntimeContextContainer()
	runtime, mappings, _ := loadNamedEndpointRuntime(t, runtimeContext)
	require.NoError(t, runtime.SetRuntimeContext(ctx, runtimeContext))
	require.True(t, runtime.Base.Runtime.IsContainerRuntime())
	runtime.NetworkMappings = mappings

	require.NoError(t, runtime.CreateRunnerEnvironment(ctx))
	t.Cleanup(func() { _ = runtime.RunnerEnvironment.Shutdown(ctx) })

	docker, ok := runtime.RunnerEnvironment.Env().(*dockerrun.DockerEnvironment)
	require.Truef(t, ok, "container runtime produced a %T runner", runtime.RunnerEnvironment.Env())

	published := map[uint32]bool{}
	for _, mapping := range docker.PortMappings() {
		published[uint32(mapping.Host)] = true
	}
	for _, mapping := range mappings {
		instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, mappings, mapping.GetEndpoint(), resources.NewContainerNetworkAccess())
		require.NoError(t, err)
		require.Truef(t, published[instance.GetPort()], "endpoint %q port %d is not published; published: %v",
			mapping.GetEndpoint().GetName(), instance.GetPort(), published)
	}
	require.Len(t, docker.PortMappings(), 3, "each endpoint's port is published exactly once")
}
