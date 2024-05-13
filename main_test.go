package main

import (
	"context"
	"encoding/json"
	"github.com/codefly-dev/core/agents"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"

	"github.com/codefly-dev/core/resources"

	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
)

func TestCreateToRun(t *testing.T) {
	wool.SetGlobalLogLevel(wool.DEBUG)
	agents.LogToConsole()

	ctx := context.Background()
	tmpDir := t.TempDir()
	defer func(path string) {
		err := os.RemoveAll(path)
		if err != nil {
			t.Fatal(err)
		}
	}(tmpDir)

	service := resources.Service{Name: "svc", Application: "app", Project: "project"}
	err := service.SaveAtDir(ctx, tmpDir)
	require.NoError(t, err)
	identity := &basev0.ServiceIdentity{
		Name:        "svc",
		Application: "app",
		Location:    tmpDir,
	}
	builder := NewBuilder()

	resp, err := builder.Load(ctx, &builderv0.LoadRequest{Identity: identity, CreationMode: &builderv0.CreationMode{Communicate: false}})
	require.NoError(t, err)
	require.NotNil(t, resp)

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	// Now run it
	runtime := NewRuntime()

	_, err = runtime.Load(ctx, &runtimev0.LoadRequest{Identity: identity, Environment: shared.Must(resources.LocalEnvironment().Proto()), DisableCatch: true})
	require.NoError(t, err)

	require.Equal(t, 2, len(runtime.Endpoints))

	// Create temporary network mappings
	networkManager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	networkManager.WithTemporaryPorts()

	networkMappings, err := networkManager.GenerateNetworkMappings(ctx, runtime.Base.Service, runtime.Endpoints, nil)
	require.NoError(t, err)
	require.NotNil(t, networkMappings)
	require.Equal(t, 2, len(networkMappings))

	init, err := runtime.Init(ctx, &runtimev0.InitRequest{ProposedNetworkMappings: networkMappings})
	require.NoError(t, err)
	require.NotNil(t, init)

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, init.NetworkMappings, runtime.GrpcEndpoint, resources.NewNativeNetworkAccess())
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

		// HTTP
		response, err := client.Get(instance.Address)
		// Check that we should have JSON Version: 0.0.0
		require.NoError(t, err)

		defer response.Body.Close()

		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)

		var data map[string]interface{}
		err = json.Unmarshal(body, &data)
		require.NoError(t, err)

		version, ok := data["Version"].(string)
		require.True(t, ok)
		require.Equal(t, "0.0.0", version)

		tries++
		time.Sleep(time.Second)

	}

}
