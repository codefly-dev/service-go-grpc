package adapters

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"codefly-base/pkg/gen"
	"codefly-base/pkg/gen/genconnect"
	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestConnectOnlyServesEveryProtocolAndHealth prevents a missing raw gRPC
// endpoint from requiring another listener. Generated clients reach the same
// configured implementation and interceptor, including health, on one port.
func TestConnectOnlyServesEveryProtocolAndHealth(t *testing.T) {
	for _, dialled := range []bool{false, true} {
		t.Run(fmt.Sprintf("dialled=%v", dialled), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := uint16(listener.Addr().(*net.TCPAddr).Port)
			listener.Close()
			config := &Configuration{EndpointConnectPort: &port, Service: &singleListenerService{}, GRPCServerOptions: []grpc.ServerOption{
				grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
					if info.FullMethod == gen.WebService_Version_FullMethodName {
						md, _ := metadata.FromIncomingContext(ctx)
						if len(md.Get("authorization")) == 0 {
							return nil, status.Error(codes.PermissionDenied, "authorization is required")
						}
					}
					return handler(ctx, req)
				}),
			}}
			if dialled {
				config.GatewayDialOptions = []grpc.DialOption{grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(8 << 20))}
			}
			server, err := NewServer(config)
			if err != nil {
				t.Fatal(err)
			}
			started := make(chan error, 1)
			go func() { started <- server.Start(t.Context()) }()
			t.Cleanup(func() {
				server.Stop()
				select {
				case err := <-started:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("Connect-only server did not stop")
				}
			})
			target := fmt.Sprintf("127.0.0.1:%d", port)
			client := &http.Client{Timeout: 2 * time.Second}
			connectClient := genconnect.NewWebServiceClient(client, "http://"+target)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			for {
				req := connect.NewRequest(&gen.VersionRequest{})
				req.Header().Set("Authorization", "test-caller")
				response, err := connectClient.Version(ctx, req)
				if err == nil {
					if response.Msg.Version != "single-listener" {
						t.Fatalf("wrong implementation: %v", response.Msg)
					}
					break
				}
				if ctx.Err() != nil {
					t.Fatalf("Connect listener did not become ready: %v", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			for _, options := range [][]connect.ClientOption{nil, {connect.WithGRPCWeb()}} {
				c := genconnect.NewWebServiceClient(client, "http://"+target, options...)
				req := connect.NewRequest(&gen.VersionRequest{})
				req.Header().Set("Authorization", "test-caller")
				resp, err := c.Version(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				if resp.Msg.Version != "single-listener" {
					t.Fatal(resp.Msg)
				}
				_, err = c.Version(ctx, connect.NewRequest(&gen.VersionRequest{}))
				if connect.CodeOf(err) != connect.CodePermissionDenied {
					t.Fatalf("interceptor bypassed: %v", err)
				}
			}
			conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			grpcClient := gen.NewWebServiceClient(conn)
			response, err := grpcClient.Version(metadata.AppendToOutgoingContext(ctx, "authorization", "test-caller"), &gen.VersionRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if response.Version != "single-listener" {
				t.Fatal(response)
			}
			_, err = grpcClient.Version(ctx, &gen.VersionRequest{})
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("gRPC interceptor bypassed: %v", err)
			}
			for _, name := range []string{"", gen.WebService_ServiceDesc.ServiceName} {
				health, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: name})
				if err != nil {
					t.Fatal(err)
				}
				if health.Status != grpc_health_v1.HealthCheckResponse_SERVING {
					t.Fatal(health)
				}
			}
			select {
			case err := <-started:
				t.Fatalf("Start returned while listener was serving: %v", err)
			default:
			}
		})
	}
}

type singleListenerService struct {
	gen.UnimplementedWebServiceServer
}

func (*singleListenerService) Version(context.Context, *gen.VersionRequest) (*gen.VersionResponse, error) {
	return &gen.VersionResponse{Version: "single-listener"}, nil
}
