package adapters

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"codefly-base/pkg/gen"
	"codefly-base/pkg/gen/genconnect"
	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// configuredService stands in for a real service's implementation: it is
// installed through Configuration.Service, and it answers from the request
// context rather than from a constant, so a response proves both that this
// implementation ran and that the caller's headers reached it.
type configuredService struct {
	gen.UnimplementedWebServiceServer
}

func (c *configuredService) Version(ctx context.Context, req *gen.VersionRequest) (*gen.VersionResponse, error) {
	caller := "anonymous"
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if values := md.Get("x-caller"); len(values) > 0 {
			caller = values[0]
		}
	}
	return &gen.VersionResponse{Version: "configured-service/" + caller}, nil
}

// refusalDetail is carried in the error details of the guard's refusal below. A
// service's API contract can put meaning in error details — typically a
// google.rpc.ErrorInfo reason — so the transport has to preserve them, not just
// the code. Any proto message travels the same way (as a packed Any), and this
// one keeps the generated fixture's dependencies to what a fresh service
// declares.
const refusalDetail = "missing-authority"

// authorityGuard is the shape of the interceptors a service installs through
// Configuration.GRPCServerOptions — an authorization check keyed on the method,
// refusing with a gRPC status that carries details.
func authorityGuard() grpc.ServerOption {
	return grpc.UnaryInterceptor(func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		if len(md.Get("authorization")) > 0 {
			return handler(ctx, request)
		}
		refusal, err := status.New(codes.PermissionDenied, "no authority for "+info.FullMethod).
			WithDetails(&gen.VersionResponse{Version: refusalDetail})
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		return nil, refusal.Err()
	})
}

// startServedService boots the whole generated server — gRPC, REST and Connect
// listeners — with a configured service and the given server options, and
// returns the Connect listener's base URL.
func startServedService(t *testing.T, options ...grpc.ServerOption) (string, *http.Client) {
	t.Helper()
	ports := unusedPorts(t, 3)
	config := &Configuration{
		EndpointGrpcPort:    ports[0],
		EndpointHttpPort:    portPointer(ports[1]),
		EndpointConnectPort: portPointer(ports[2]),
		GRPCServerOptions:   options,
		Service:             &configuredService{},
	}
	server, err := NewServer(config)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	started := make(chan error, 1)
	go func() { started <- server.Start(context.Background()) }()
	t.Cleanup(func() {
		server.Stop()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", ports[2])
	waitForHTTP(t, base+"/api.WebService/")
	return base, &http.Client{Timeout: 10 * time.Second}
}

// h2cClient speaks HTTP/2 over cleartext, which is what the gRPC protocol
// requires of a caller — the listener serves it through h2c.NewHandler, and a
// gRPC request over HTTP/1.1 is refused rather than downgraded.
func h2cClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			},
		},
	}
}

// TestConnectListenerDispatchesConfiguredService is the regression for the
// listener answering `unimplemented` for every substantive RPC: it used to
// install a Connect handler of its own that implemented Version and inherited
// the rest as unimplemented, while Configuration.Service — the implementation
// with the service's real dependencies — was reachable on the gRPC listener
// alone. A Connect caller could therefore reach a service that was running and
// be told its API did not exist.
func TestConnectListenerDispatchesConfiguredService(t *testing.T) {
	base, httpClient := startServedService(t)

	for _, protocol := range []struct {
		name    string
		options []connect.ClientOption
		http2   bool
	}{
		{name: "connect+proto"},
		{name: "connect+json", options: []connect.ClientOption{connect.WithProtoJSON()}},
		// A client that compresses its request: gRPC refuses an encoding it has
		// no decompressor registered for, and refuses it as Unimplemented, so
		// the transcoder has to decompress before dispatching.
		{name: "connect+gzip", options: []connect.ClientOption{connect.WithSendGzip()}},
		{name: "grpc-web", options: []connect.ClientOption{connect.WithGRPCWeb()}},
		{name: "grpc", options: []connect.ClientOption{connect.WithGRPC()}, http2: true},
	} {
		t.Run(protocol.name, func(t *testing.T) {
			caller := httpClient
			if protocol.http2 {
				caller = h2cClient()
			}
			client := genconnect.NewWebServiceClient(caller, base, protocol.options...)
			request := connect.NewRequest(&gen.VersionRequest{})
			request.Header().Set("X-Caller", protocol.name)
			response, err := client.Version(context.Background(), request)
			if err != nil {
				if connect.CodeOf(err) == connect.CodeUnimplemented {
					t.Fatalf("the Connect listener does not dispatch the configured service: %v", err)
				}
				t.Fatalf("call Version: %v", err)
			}
			if want := "configured-service/" + protocol.name; response.Msg.GetVersion() != want {
				t.Fatalf("Version = %q, want %q: the configured implementation did not answer, or the request headers did not reach it", response.Msg.GetVersion(), want)
			}
		})
	}
}

// TestConnectListenerAppliesGRPCTransportPolicy keeps the two listeners under
// one policy. The Connect listener dispatches through the gRPC server, so every
// Configuration.GRPCServerOptions entry — the authority and operations
// interceptors a service installs, and its message bounds — applies to a
// Connect caller too. A Connect path that dispatched a service directly would
// answer the same RPC with none of them, which is an authority hole that looks
// like a working endpoint.
func TestConnectListenerAppliesGRPCTransportPolicy(t *testing.T) {
	const receiveBound = 1 << 10
	base, httpClient := startServedService(t, authorityGuard(), grpc.MaxRecvMsgSize(receiveBound))
	client := genconnect.NewWebServiceClient(httpClient, base)

	// The installed guard must refuse an unauthorized Connect call, and its gRPC
	// status must arrive as the equivalent Connect error, details included.
	_, err := client.Version(context.Background(), connect.NewRequest(&gen.VersionRequest{}))
	if err == nil {
		t.Fatal("an unauthorized Connect call was served: the interceptor did not run")
	}
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("refusal code = %v, want permission_denied (unimplemented means no dispatch at all): %v", got, err)
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("refusal is not a connect error: %v", err)
	}
	var details []string
	for _, detail := range connectErr.Details() {
		value, valueErr := detail.Value()
		if valueErr != nil {
			t.Fatalf("decode error detail: %v", valueErr)
		}
		if carried, ok := value.(*gen.VersionResponse); ok {
			details = append(details, carried.GetVersion())
		}
	}
	if len(details) != 1 || details[0] != refusalDetail {
		t.Fatalf("error details = %v, want the one detail the refusal carried (%q): details are dropped in translation", details, refusalDetail)
	}

	// With authority the same call is served.
	authorized := connect.NewRequest(&gen.VersionRequest{})
	authorized.Header().Set("Authorization", "Bearer test")
	authorized.Header().Set("X-Caller", "guarded")
	if _, err := client.Version(context.Background(), authorized); err != nil {
		t.Fatalf("authorized call: %v", err)
	}

	// A request over the configured receive bound must be refused by it. The
	// payload is one unknown length-delimited field (15), which proto3 would
	// otherwise skip without complaint, so the bound is the only thing that can
	// reject it.
	oversized := make([]byte, 0, 4+(64<<10))
	oversized = append(oversized, 0x7a, 0x80, 0x80, 0x04)
	oversized = append(oversized, make([]byte, 64<<10)...)
	request, err := http.NewRequest(http.MethodPost, base+"/api.WebService/Version", bytes.NewReader(oversized))
	if err != nil {
		t.Fatalf("build oversized request: %v", err)
	}
	request.Header.Set("Content-Type", "application/proto")
	request.Header.Set("Connect-Protocol-Version", "1")
	request.Header.Set("Authorization", "Bearer test")
	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatalf("send oversized request: %v", err)
	}
	defer response.Body.Close()
	var refusal struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(response.Body).Decode(&refusal); err != nil {
		t.Fatalf("decode oversized refusal: %v", err)
	}
	if refusal.Code != "resource_exhausted" {
		t.Fatalf("oversized request answered %q (HTTP %d), want resource_exhausted: the receive bound does not apply on the Connect listener", refusal.Code, response.StatusCode)
	}

	// The same payload compressed. It is a few hundred bytes on the wire and
	// inflates past the bound, so it is only refused if the bound is applied to
	// the decompressed message — which is why the listener lets gRPC do the
	// decompressing rather than inflating the body itself.
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(oversized); err != nil {
		t.Fatalf("compress oversized payload: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("finish compressing oversized payload: %v", err)
	}
	if compressed.Len() >= receiveBound {
		t.Fatalf("compressed payload is %d bytes, which the bound (%d) refuses before inflating it", compressed.Len(), receiveBound)
	}
	request, err = http.NewRequest(http.MethodPost, base+"/api.WebService/Version", bytes.NewReader(compressed.Bytes()))
	if err != nil {
		t.Fatalf("build compressed request: %v", err)
	}
	request.Header.Set("Content-Type", "application/proto")
	request.Header.Set("Connect-Protocol-Version", "1")
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("Authorization", "Bearer test")
	response, err = httpClient.Do(request)
	if err != nil {
		t.Fatalf("send compressed oversized request: %v", err)
	}
	defer response.Body.Close()
	refusal.Code = ""
	if err := json.NewDecoder(response.Body).Decode(&refusal); err != nil {
		t.Fatalf("decode compressed oversized refusal: %v", err)
	}
	if refusal.Code != "resource_exhausted" {
		t.Fatalf("compressed oversized request answered %q (HTTP %d), want resource_exhausted: the receive bound is not applied to the decompressed message", refusal.Code, response.StatusCode)
	}
}

// TestConnectListenerServesRPCProtocolsOnly pins this listener's surface. The
// transcoder it is built on can also answer a service's google.api.http REST
// paths, but those belong to the REST listener, which wraps them in the CORS
// policy and the body logging this port has neither of; mounting the transcoder
// at the root would have published a second, unprotected REST surface. Every
// service registered on the gRPC server — health included — stays reachable.
func TestConnectListenerServesRPCProtocolsOnly(t *testing.T) {
	base, httpClient := startServedService(t)

	// api.WebService.Version is annotated `get: "/version"`; that route is the
	// REST listener's alone.
	response, err := httpClient.Get(base + "/version")
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /version on the Connect listener = %d, want 404: the REST surface is published here too", response.StatusCode)
	}

	// The health service is registered on the same gRPC server, so a Connect
	// caller — a probe, or a mesh health check — reaches it as well.
	health, err := http.NewRequest(http.MethodPost, base+"/grpc.health.v1.Health/Check", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("build health request: %v", err)
	}
	health.Header.Set("Content-Type", "application/json")
	health.Header.Set("Connect-Protocol-Version", "1")
	healthResponse, err := httpClient.Do(health)
	if err != nil {
		t.Fatalf("health check over Connect: %v", err)
	}
	defer healthResponse.Body.Close()
	var served struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(healthResponse.Body).Decode(&served); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if served.Status != "SERVING" {
		t.Fatalf("health over Connect = %q (HTTP %d), want SERVING", served.Status, healthResponse.StatusCode)
	}
}
