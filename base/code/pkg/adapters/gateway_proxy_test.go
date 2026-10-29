package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"codefly-base/pkg/gen"
	"codefly-base/pkg/gen/genconnect"

	"connectrpc.com/connect"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// A wildcard self-dial is eligible for environment proxying, exposing caller
// metadata and messages. Run each case in a fresh process because gRPC caches
// proxy environment settings. The control proves the recorder is reachable;
// the options case independently pins WithNoProxy even though loopback alone
// currently bypasses Go's proxy selection.
func TestGatewaySelfDialsNeverContactEnvironmentProxy(t *testing.T) {
	if mode := os.Getenv("CODEFLY_GATEWAY_PROXY_TEST"); mode != "" {
		runGatewayProxyCase(t, mode)
		return
	}
	for _, mode := range []string{"control", "options", "gateways"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var contacts atomic.Int64
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					contacts.Add(1)
					_ = conn.Close()
				}
			}()
			t.Cleanup(func() { _ = listener.Close(); <-done })
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGatewaySelfDialsNeverContactEnvironmentProxy$", "-test.v")
			command.Env = append(os.Environ(),
				"CODEFLY_GATEWAY_PROXY_TEST="+mode,
				"HTTPS_PROXY=http://"+listener.Addr().String(),
				"https_proxy=http://"+listener.Addr().String(),
				"HTTP_PROXY=http://"+listener.Addr().String(),
				"http_proxy=http://"+listener.Addr().String(),
				"NO_PROXY=", "no_proxy=")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Errorf("%s: %v\n%s", mode, err, output)
			}
			if got := contacts.Load(); mode == "control" {
				if got == 0 {
					t.Error("vulnerable wildcard dial never reached the recording proxy")
				}
			} else if got != 0 {
				t.Errorf("%s contacted the environment proxy %d times", mode, got)
			}
		})
	}
}

func runGatewayProxyCase(t *testing.T, mode string) {
	t.Helper()
	if mode != "gateways" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := grpc.NewServer()
		gen.RegisterWebServiceServer(server, &configuredService{})
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(server.Stop)
		options := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
		if mode == "options" {
			options = gatewayDialOptions()
		}
		conn, err := grpc.NewClient(fmt.Sprintf("0.0.0.0:%d", listener.Addr().(*net.TCPAddr).Port), options...)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		response, err := gen.NewWebServiceClient(conn).Version(ctx, &gen.VersionRequest{})
		if mode == "control" {
			if err == nil {
				t.Fatal("vulnerable dial unexpectedly bypassed the recording proxy")
			}
		} else if err != nil || response.GetVersion() != "configured-service/anonymous" {
			t.Fatalf("proxy-free options could not reach service: %v, %v", response, err)
		}
		return
	}

	// A nonempty option list selects Connect's network transcoder, without a
	// custom dialer that would itself bypass gRPC's environment proxy logic.
	config := &Configuration{
		GatewayDialOptions: []grpc.DialOption{grpc.WithUserAgent("gateway-proxy-test")},
		// grpc-gateway 2.33 refuses arbitrary Grpc-Metadata-* injection. The
		// proxy test carries only its explicitly configured fixture metadata.
		ServeMuxOptions: []runtime.ServeMuxOption{runtime.WithMetadata(func(_ context.Context, req *http.Request) metadata.MD {
			return metadata.Pairs("x-caller", req.Header.Get("X-Caller"))
		})},
	}
	base, httpClient := startConfiguredService(t, config)
	client := genconnect.NewWebServiceClient(httpClient, base)
	request := connect.NewRequest(&gen.VersionRequest{})
	request.Header().Set("X-Caller", "proxy-test")
	response, err := client.Version(t.Context(), request)
	if err != nil || response.Msg.GetVersion() != "configured-service/proxy-test" {
		t.Fatalf("Connect self-dial: %v, %v", response, err)
	}

	restBase := fmt.Sprintf("http://127.0.0.1:%d", *config.EndpointHttpPort)
	waitForHTTP(t, restBase+"/healthz")
	served := restGet(t, restBase+"/version", http.Header{"X-Caller": {"proxy-test"}})
	var version gen.VersionResponse
	if err := json.Unmarshal([]byte(restBody(t, served)), &version); err != nil {
		t.Fatal(err)
	}
	if served.StatusCode != http.StatusOK || version.GetVersion() != "configured-service/proxy-test" {
		t.Fatalf("REST self-dial: status %d, version %q", served.StatusCode, version.GetVersion())
	}
	health := restGet(t, restBase+"/healthz", nil)
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health self-dial: status %d: %s", health.StatusCode, restBody(t, health))
	}
}
