package adapters

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// startREST boots the generated server with config over real listeners and
// returns the REST base URL. The extension points are exercised through the
// server the agent generates, not a hand-assembled mux: a seam that only works
// in a copy of Run is the defect these tests exist to catch.
func startREST(t *testing.T, config *Configuration) string {
	t.Helper()
	server, err := NewServer(config)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	started := make(chan error, 1)
	go func() { started <- server.Start(context.Background()) }()
	t.Cleanup(func() {
		server.Stop()
		select {
		case err := <-started:
			if err != nil {
				t.Errorf("stop server: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", *config.EndpointHttpPort)
	waitForHTTP(t, base+"/healthz")
	return base
}

func restConfiguration(t *testing.T) *Configuration {
	t.Helper()
	ports := unusedPorts(t, 2)
	return &Configuration{
		EndpointGrpcPort: ports[0],
		EndpointHttpPort: portPointer(ports[1]),
	}
}

func restGet(t *testing.T, url string, header http.Header) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", url, err)
	}
	for name, values := range header {
		request.Header[name] = values
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func restBody(t *testing.T, response *http.Response) string {
	t.Helper()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(content)
}

// TestRESTGatewayAppliesServiceServeMuxOptions holds the seam #154 reports
// missing: a runtime.ServeMuxOption only reaches a mux at construction, and the
// plugins.RegisterREST seam receives the mux already built, so without
// Configuration.ServeMuxOptions a service could not add one at all — and the one
// downstream service that needed HTTP caching on a single RPC hand-edited the
// generated file, which the next sync silently reverted.
//
// The pair is the motivating case end to end: a WithMetadata annotator carries
// the request's If-None-Match inward, and a WithForwardResponseOption answers
// with a strong ETag and a Cache-Control, or a bare 304 when the caller already
// holds the entity.
func TestRESTGatewayAppliesServiceServeMuxOptions(t *testing.T) {
	const etag = `"v1"`

	config := restConfiguration(t)
	config.ServeMuxOptions = []runtime.ServeMuxOption{
		runtime.WithMetadata(func(_ context.Context, request *http.Request) metadata.MD {
			if tag := request.Header.Get("If-None-Match"); tag != "" {
				return metadata.Pairs("x-if-none-match", tag)
			}
			return nil
		}),
		runtime.WithForwardResponseOption(func(ctx context.Context, w http.ResponseWriter, _ proto.Message) error {
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
			if md, ok := metadata.FromOutgoingContext(ctx); ok {
				for _, held := range md.Get("x-if-none-match") {
					if held == etag {
						w.WriteHeader(http.StatusNotModified)
						return nil
					}
				}
			}
			return nil
		}),
	}
	base := startREST(t, config)

	served := restGet(t, base+"/version", nil)
	if served.StatusCode != http.StatusOK {
		t.Fatalf("GET /version: %d %s", served.StatusCode, restBody(t, served))
	}
	if got := served.Header.Get("ETag"); got != etag {
		t.Errorf("the service's forward-response option did not run on the gateway response: ETag %q, want %q", got, etag)
	}
	if got := served.Header.Get("Cache-Control"); got == "" {
		t.Error("the service's forward-response option set no Cache-Control on the gateway response")
	}

	revalidated := restGet(t, base+"/version", http.Header{"If-None-Match": []string{etag}})
	if revalidated.StatusCode != http.StatusNotModified {
		t.Fatalf("a revalidated read must answer 304: %d %s", revalidated.StatusCode, restBody(t, revalidated))
	}
}

// TestRESTServiceServeMuxOptionOverridesAGeneratedOne pins the ordering the
// Configuration.ServeMuxOptions comment promises, for the class of option where
// order decides the outcome: grpc-gateway's WithErrorHandler sets one field, so
// applying the service's options after the generated ones is what makes it
// replace customErrorHandler rather than be silently ignored. (The accumulating
// class — WithMetadata, WithForwardResponseOption — appends instead, which is
// why the generated annotator still runs beside the one the test above adds.)
func TestRESTServiceServeMuxOptionOverridesAGeneratedOne(t *testing.T) {
	config := restConfiguration(t)
	config.ServeMuxOptions = []runtime.ServeMuxOption{
		runtime.WithErrorHandler(func(_ context.Context, _ *runtime.ServeMux, _ runtime.Marshaler, w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte("service error handler"))
		}),
	}
	base := startREST(t, config)

	// An unrouted path reaches the mux's error handler, which the generated
	// options set to customErrorHandler ("Route not found", 404).
	response := restGet(t, base+"/no-such-route", nil)
	if response.StatusCode != http.StatusTeapot {
		t.Fatalf("the generated error handler still answers: %d %s", response.StatusCode, restBody(t, response))
	}
	if content := restBody(t, response); content != "service error handler" {
		t.Errorf("error body %q, want the service's", content)
	}
}

// TestRESTRoutesAreServedAheadOfTheGateway holds the second seam #154 reports:
// an HTTP handler for a protocol the gateway cannot carry — a Connect handler
// for a second protobuf service, whose every method would otherwise need its
// own gwMux.HandlePath template — mounted by prefix on the listener the agent
// already runs, with every path outside the prefix reaching the gateway
// unchanged.
func TestRESTRoutesAreServedAheadOfTheGateway(t *testing.T) {
	config := restConfiguration(t)
	config.Routes = []Route{{
		Prefix: "/pkg.v1.OtherService/",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("routed " + r.URL.Path))
		}),
	}}
	base := startREST(t, config)

	routed := restGet(t, base+"/pkg.v1.OtherService/DoThing", nil)
	if routed.StatusCode != http.StatusOK {
		t.Fatalf("GET the mounted prefix: %d %s", routed.StatusCode, restBody(t, routed))
	}
	// The path arrives exactly as sent: the route is a plain prefix match, not
	// an http.ServeMux that would clean and redirect it.
	if content := restBody(t, routed); content != "routed /pkg.v1.OtherService/DoThing" {
		t.Errorf("mounted handler saw %q", content)
	}

	// A path outside every prefix still reaches the gateway, and the gateway
	// still answers it from the gRPC service.
	version := restGet(t, base+"/version", nil)
	if version.StatusCode != http.StatusOK {
		t.Fatalf("GET /version behind a mounted route: %d %s", version.StatusCode, restBody(t, version))
	}
	if content := restBody(t, version); !strings.Contains(content, `"version"`) {
		t.Errorf("gateway response %q does not carry the version field", content)
	}

	// Including the health route the deployment probes, which Run registers on
	// the gateway mux after the routes are consulted.
	health := restGet(t, base+"/healthz", nil)
	if health.StatusCode != http.StatusOK || !strings.Contains(restBody(t, health), "SERVING") {
		t.Errorf("GET /healthz behind a mounted route: %d", health.StatusCode)
	}
}
