package adapters

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"codefly-base/pkg/gen"
	"codefly-base/pkg/gen/genconnect"
	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
)

// shortTimeout stands in for the Connect listener's 10 s, 30 s and 120 s
// defaults, so a test can wait past it.
const shortTimeout = 300 * time.Millisecond

// cutOffWithin is how long a cut-off may take: the timeout under test plus
// scheduling, well under the seconds a dripping caller keeps sending for.
const cutOffWithin = 3 * time.Second

// startTimedFace serves the generated Connect listener, built by
// NewConnectServer from config, over a gRPC server that also holds the
// reflection service, whose ServerReflectionInfo is a bidirectional stream; the
// health service's Watch, registered by NewGrpServer, is a server stream. It
// returns the listener's address and the gRPC server.
func startTimedFace(t *testing.T, config *Configuration) (string, *GrpcServer) {
	t.Helper()
	ports := unusedPorts(t, 2)
	config.EndpointGrpcPort = ports[0]
	config.EndpointConnectPort = portPointer(ports[1])
	if config.Service == nil {
		config.Service = &configuredService{}
	}
	grpcServer, err := NewGrpServer(config)
	if err != nil {
		t.Fatalf("create gRPC server: %v", err)
	}
	reflection.Register(grpcServer.gRPC)
	face, err := NewConnectServer(config, grpcServer.gRPC)
	if err != nil {
		t.Fatalf("create Connect listener: %v", err)
	}
	go func() { _ = face.Run(context.Background()) }()
	t.Cleanup(func() { _ = face.server.Close(); grpcServer.gRPC.Stop() })
	address := fmt.Sprintf("127.0.0.1:%d", ports[1])
	waitForHTTP(t, "http://"+address+"/api.WebService/")
	return address, grpcServer
}

// slowService answers Version after delay, or with the context's error if the
// call is cancelled first.
type slowService struct {
	gen.UnimplementedWebServiceServer
	delay time.Duration
}

func (s *slowService) Version(ctx context.Context, _ *gen.VersionRequest) (*gen.VersionResponse, error) {
	select {
	case <-time.After(s.delay):
		return &gen.VersionResponse{Version: "slow"}, nil
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}

// h2cConn opens an HTTP/2 connection with prior knowledge, the way a gRPC
// client reaches the listener, and returns its framer.
func h2cConn(t *testing.T, address string) (net.Conn, *http2.Framer) {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	framer := http2.NewFramer(conn, conn)
	if err := framer.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	return conn, framer
}

// awaitClose reads conn until the server closes it and returns how long that
// took, failing if it is still open after cutOffWithin.
func awaitClose(t *testing.T, conn net.Conn, start time.Time) (time.Duration, []byte) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(cutOffWithin + time.Second))
	answer, err := io.ReadAll(conn)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("the connection was still open after %v", time.Since(start))
	}
	return time.Since(start), answer
}

// TestConnectListenerCutsOffADrippedBody is the regression for the generated
// Connect listener setting no read timeout: a caller that sends its headers and
// then drips a unary body, with no deadline of its own, held the connection and
// the buffer behind it for as long as it kept dripping. The body is bounded by
// ConnectReadTimeout now — over HTTP/1.1 the connection ends, over h2c (the
// gRPC protocol's transport on this port) the stream ends — and neither is cut
// before the timeout, which is what shows the timeout did it.
func TestConnectListenerCutsOffADrippedBody(t *testing.T) {
	t.Run("http1", func(t *testing.T) {
		address, _ := startTimedFace(t, &Configuration{ConnectReadTimeout: shortTimeout})
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := fmt.Fprintf(conn, "POST /api.WebService/Version HTTP/1.1\r\nHost: web\r\nContent-Type: application/proto\r\nConnect-Protocol-Version: 1\r\nContent-Length: 100000\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		go drip(func() error { _, err := conn.Write([]byte{0}); return err })
		elapsed, answer := awaitClose(t, conn, start)
		if elapsed > cutOffWithin {
			t.Fatalf("cut off only after %v", elapsed)
		}
		if elapsed < shortTimeout/2 {
			t.Fatalf("closed after %v, before the read timeout could have fired: %q", elapsed, answer)
		}
		if strings.Contains(string(answer), " 200 OK") {
			t.Fatalf("a dripped body was answered: %q", answer)
		}
	})
	t.Run("h2c", func(t *testing.T) {
		address, _ := startTimedFace(t, &Configuration{ConnectReadTimeout: shortTimeout})
		conn, framer := h2cConn(t, address)
		var block bytes.Buffer
		encoder := hpack.NewEncoder(&block)
		for _, field := range [][2]string{{":method", "POST"}, {":scheme", "http"}, {":path", "/api.WebService/Version"},
			{":authority", "web"}, {"content-type", "application/grpc"}, {"te", "trailers"}} {
			_ = encoder.WriteField(hpack.HeaderField{Name: field[0], Value: field[1]})
		}
		if err := framer.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block.Bytes(), EndHeaders: true}); err != nil {
			t.Fatal(err)
		}
		prefix := make([]byte, 5)
		binary.BigEndian.PutUint32(prefix[1:], 1000) // one message of 1000 bytes, under every bound
		if err := framer.WriteData(1, false, prefix); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		go drip(func() error { return framer.WriteData(1, false, []byte{0}) })
		_ = conn.SetReadDeadline(time.Now().Add(cutOffWithin + time.Second))
		for ended := false; !ended; {
			frame, err := framer.ReadFrame()
			if err != nil {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					t.Fatalf("the dripping stream was still open after %v", time.Since(start))
				}
				break // the connection ended
			}
			switch frame := frame.(type) {
			case *http2.RSTStreamFrame:
				ended = frame.StreamID == 1
			case *http2.HeadersFrame:
				ended = frame.StreamID == 1 && frame.StreamEnded()
			case *http2.DataFrame:
				ended = frame.StreamID == 1 && frame.StreamEnded()
			}
		}
		elapsed := time.Since(start)
		if elapsed > cutOffWithin {
			t.Fatalf("cut off only after %v", elapsed)
		}
		if elapsed < shortTimeout/2 {
			t.Fatalf("the stream ended after %v, before the read timeout could have fired", elapsed)
		}
	})
}

// drip sends one byte every 25 ms for five seconds, or until send fails.
func drip(send func() error) {
	for range 200 {
		if send() != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestConnectListenerServesPromptCallers: a caller that sends its request at
// once is served even when the handler answers well after the read timeout,
// over HTTP/1.1 and h2c, and the next call on the same HTTP/1.1 connection is
// served too. The timeout bounds sending the body, not answering it: a
// server-wide read deadline on HTTP/1.1 would cancel the call's context once
// it passed, even with the body long read.
func TestConnectListenerServesPromptCallers(t *testing.T) {
	address, _ := startTimedFace(t, &Configuration{
		ConnectReadTimeout: shortTimeout,
		Service:            &slowService{delay: 3 * shortTimeout},
	})
	base := "http://" + address
	for _, protocol := range []struct {
		name   string
		client *http.Client
		option []connect.ClientOption
	}{
		{name: "connect over HTTP/1.1", client: &http.Client{Timeout: 10 * time.Second}},
		{name: "grpc over h2c", client: h2cClient(), option: []connect.ClientOption{connect.WithGRPC()}},
	} {
		t.Run(protocol.name, func(t *testing.T) {
			client := genconnect.NewWebServiceClient(protocol.client, base, protocol.option...)
			for call := range 2 {
				response, err := client.Version(context.Background(), connect.NewRequest(&gen.VersionRequest{}))
				if err != nil {
					t.Fatalf("call %d, answered after the read timeout, failed: %v", call, err)
				}
				if response.Msg.GetVersion() != "slow" {
					t.Fatalf("call %d answered %q", call, response.Msg.GetVersion())
				}
			}
		})
	}
}

// TestConnectListenerLeavesStreamsUncut: the read timeout never cuts a stream.
// A server stream (health Watch) delivers an update sent after the timeout,
// over Connect on HTTP/1.1 and over gRPC on h2c, because its one request
// message ended within it; a bidirectional stream (reflection) whose caller
// sends again after the timeout is answered, because a caller-streamed body is
// not bounded at all.
func TestConnectListenerLeavesStreamsUncut(t *testing.T) {
	address, grpcServer := startTimedFace(t, &Configuration{ConnectReadTimeout: shortTimeout})
	base := "http://" + address
	for _, protocol := range []struct {
		name   string
		client *http.Client
		option []connect.ClientOption
	}{
		{name: "connect over HTTP/1.1", client: &http.Client{}},
		{name: "grpc over h2c", client: h2cClient(), option: []connect.ClientOption{connect.WithGRPC()}},
	} {
		t.Run("server stream, "+protocol.name, func(t *testing.T) {
			watched := strings.ReplaceAll(protocol.name, " ", "-")
			client := connect.NewClient[grpc_health_v1.HealthCheckRequest, grpc_health_v1.HealthCheckResponse](
				protocol.client, base+"/grpc.health.v1.Health/Watch", protocol.option...)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stream, err := client.CallServerStream(ctx, connect.NewRequest(&grpc_health_v1.HealthCheckRequest{Service: watched}))
			if err != nil {
				t.Fatalf("watch: %v", err)
			}
			defer stream.Close()
			if !stream.Receive() {
				t.Fatalf("no first status: %v", stream.Err())
			}
			time.Sleep(3 * shortTimeout)
			grpcServer.health.SetServingStatus(watched, grpc_health_v1.HealthCheckResponse_SERVING)
			if !stream.Receive() {
				t.Fatalf("the server stream was cut after the read timeout: %v", stream.Err())
			}
			if got := stream.Msg().GetStatus(); got != grpc_health_v1.HealthCheckResponse_SERVING {
				t.Fatalf("status after the timeout = %v, want SERVING", got)
			}
		})
	}
	t.Run("bidirectional stream, grpc over h2c", func(t *testing.T) {
		conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stream, err := reflectionpb.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ask := &reflectionpb.ServerReflectionRequest{MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{}}
		for round := range 2 {
			if round > 0 {
				time.Sleep(3 * shortTimeout)
			}
			if err := stream.Send(ask); err != nil {
				t.Fatalf("round %d send: %v", round, err)
			}
			if _, err := stream.Recv(); err != nil {
				t.Fatalf("round %d: the bidirectional stream was cut: %v", round, err)
			}
		}
		_ = stream.CloseSend()
	})
}

// TestConnectListenerCutsOffDrippedHeaders: a caller that drips an HTTP/1.1
// request's headers is cut off by ConnectReadHeaderTimeout.
func TestConnectListenerCutsOffDrippedHeaders(t *testing.T) {
	address, _ := startTimedFace(t, &Configuration{ConnectReadHeaderTimeout: shortTimeout})
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "POST /api.WebService/Version HTTP/1.1\r\n"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	go drip(func() error { _, err := conn.Write([]byte("X-Drip: 0\r\n")); return err })
	if elapsed, _ := awaitClose(t, conn, start); elapsed > cutOffWithin || elapsed < shortTimeout/2 {
		t.Fatalf("dripped headers cut off after %v, want about %v", elapsed, shortTimeout)
	}
}

// TestConnectListenerClosesIdleConnections: ConnectIdleTimeout closes a
// connection with no request in flight, HTTP/1.1 keep-alive and h2c alike. The
// h2c case is the one an unset HTTP/2 idle timeout would miss: h2c serves the
// connection without the configuration that copies it from the HTTP/1.1
// server.
func TestConnectListenerClosesIdleConnections(t *testing.T) {
	address, _ := startTimedFace(t, &Configuration{ConnectIdleTimeout: shortTimeout})
	t.Run("http1 keep-alive", func(t *testing.T) {
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := fmt.Fprintf(conn, "POST /api.WebService/Version HTTP/1.1\r\nHost: web\r\nContent-Type: application/json\r\nConnect-Protocol-Version: 1\r\nContent-Length: 2\r\n\r\n{}"); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(conn)
		response, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("the call before going idle answered %d", response.StatusCode)
		}
		start := time.Now()
		_ = conn.SetReadDeadline(time.Now().Add(cutOffWithin + time.Second))
		if _, err := reader.ReadByte(); err != io.EOF {
			t.Fatalf("idle keep-alive connection: read %v after %v, want the server to close it", err, time.Since(start))
		}
		if elapsed := time.Since(start); elapsed > cutOffWithin {
			t.Fatalf("idle connection closed only after %v", elapsed)
		}
	})
	t.Run("h2c", func(t *testing.T) {
		conn, framer := h2cConn(t, address)
		start := time.Now()
		_ = conn.SetReadDeadline(time.Now().Add(cutOffWithin + time.Second))
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					t.Fatalf("the idle h2c connection was still open after %v", time.Since(start))
				}
				break
			}
			if _, ok := frame.(*http2.GoAwayFrame); ok {
				break
			}
		}
		if elapsed := time.Since(start); elapsed > cutOffWithin {
			t.Fatalf("idle h2c connection closed only after %v", elapsed)
		}
	})
}

// TestConnectListenerTimeoutDefaults: a Configuration that leaves the
// timeouts at zero gets 10 s for headers, 30 s for a body and 120 s idle, and
// a negative value removes a bound. The listener never sets a server-wide
// ReadTimeout, which would cut every client stream.
func TestConnectListenerTimeoutDefaults(t *testing.T) {
	build := func(config *Configuration) *ConnectServer {
		config.EndpointConnectPort = portPointer(0)
		grpcServer, err := NewGrpServer(config)
		if err != nil {
			t.Fatal(err)
		}
		face, err := NewConnectServer(config, grpcServer.gRPC)
		if err != nil {
			t.Fatal(err)
		}
		return face
	}
	defaults := build(&Configuration{}).server
	if defaults.ReadHeaderTimeout != 10*time.Second || defaults.IdleTimeout != 120*time.Second || defaults.ReadTimeout != 0 || defaults.WriteTimeout != 0 {
		t.Fatalf("default timeouts: header %v, idle %v, read %v, write %v; want 10s, 2m0s, 0s, 0s",
			defaults.ReadHeaderTimeout, defaults.IdleTimeout, defaults.ReadTimeout, defaults.WriteTimeout)
	}
	if got := connectTimeout(0, defaultConnectReadTimeout); got != 30*time.Second {
		t.Fatalf("default body read timeout = %v, want 30s", got)
	}
	removed := build(&Configuration{ConnectReadHeaderTimeout: -1, ConnectIdleTimeout: -1}).server
	if removed.ReadHeaderTimeout != 0 || removed.IdleTimeout != 0 {
		t.Fatalf("negative timeouts left header %v, idle %v; want no bound", removed.ReadHeaderTimeout, removed.IdleTimeout)
	}
	if got := connectTimeout(-1, defaultConnectReadTimeout); got != 0 {
		t.Fatalf("a negative body read timeout = %v, want no bound", got)
	}
}
