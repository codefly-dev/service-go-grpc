package adapters

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"codefly-base/pkg/gen"
	"codefly-base/pkg/gen/genconnect"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// frontDoor stands in for what a gateway dial option sends calls to first — a
// hop in front of the service. It answers Version itself, so an answer from it
// proves the Connect face dialled rather than dispatched in process, and it
// records what each call carried.
type frontDoor struct {
	gen.UnimplementedWebServiceServer
	mu      sync.Mutex
	carried []metadata.MD
}

func (f *frontDoor) Version(ctx context.Context, _ *gen.VersionRequest) (*gen.VersionResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.mu.Lock()
	f.carried = append(f.carried, md.Copy())
	f.mu.Unlock()
	if len(md.Get("x-refuse")) > 0 {
		refusal, _ := status.New(codes.PermissionDenied, "refused at the door").WithDetails(&gen.VersionResponse{Version: refusalDetail})
		return nil, refusal.Err()
	}
	caller := "anonymous"
	if v := md.Get("x-caller"); len(v) > 0 {
		caller = v[0]
	}
	return &gen.VersionResponse{Version: "front-door/" + caller}, nil
}

// startFrontDoor serves frontDoor on its own listener and returns a dial
// option that sends every call to it, the shape a hop's dial option has.
func startFrontDoor(t *testing.T) (*frontDoor, grpc.DialOption) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	door := &frontDoor{}
	server := grpc.NewServer()
	gen.RegisterWebServiceServer(server, door)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	address := listener.Addr().String()
	return door, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	})
}

// With gateway dial options, the Connect face reaches the methods the way the
// REST face does — by dialling, through those options — so a front door the
// options name decides every Connect call too. Every protocol the listener
// serves takes that path, carrying the caller's metadata, and a refusal keeps
// its code and details.
func TestConnectListenerDialsThroughTheGatewayDialOptions(t *testing.T) {
	door, dialOption := startFrontDoor(t)
	ports := unusedPorts(t, 3)
	server, err := NewServer(&Configuration{
		EndpointGrpcPort:    ports[0],
		EndpointHttpPort:    portPointer(ports[1]),
		EndpointConnectPort: portPointer(ports[2]),
		Service:             &configuredService{},
		GatewayDialOptions:  []grpc.DialOption{dialOption},
	})
	if err != nil {
		t.Fatal(err)
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
	httpClient := &http.Client{Timeout: 10 * time.Second}

	for _, protocol := range []struct {
		name    string
		options []connect.ClientOption
		http2   bool
	}{
		{name: "connect+proto"},
		{name: "connect+json", options: []connect.ClientOption{connect.WithProtoJSON()}},
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
				t.Fatalf("call Version: %v", err)
			}
			if want := "front-door/" + protocol.name; response.Msg.GetVersion() != want {
				t.Fatalf("Version = %q, want %q: the Connect face dispatched in process instead of dialling", response.Msg.GetVersion(), want)
			}
		})
	}

	door.mu.Lock()
	reached := len(door.carried)
	door.mu.Unlock()
	if reached != 5 {
		t.Fatalf("the front door saw %d of 5 calls", reached)
	}

	client := genconnect.NewWebServiceClient(httpClient, base)
	request := connect.NewRequest(&gen.VersionRequest{})
	request.Header().Set("X-Refuse", "yes")
	_, err = client.Version(context.Background(), request)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("a refusal at the door answered %v", err)
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || len(connectErr.Details()) != 1 {
		t.Fatalf("the refusal lost its details: %v", err)
	}
	if detail, derr := connectErr.Details()[0].Value(); derr != nil || detail.(*gen.VersionResponse).GetVersion() != refusalDetail {
		t.Fatalf("the refusal's detail = %v, %v", detail, derr)
	}
}
