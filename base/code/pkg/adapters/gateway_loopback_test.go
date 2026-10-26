package adapters

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codefly-base/pkg/gen"
	"codefly-base/pkg/gen/genconnect"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// The gateway's hop to the gRPC server is plaintext and carries every caller's
// metadata and messages. A service can hand that hop a dialer through
// GatewayDialOptions, and a dialer is free to ignore the 127.0.0.1 target it is
// given, so the target alone cannot keep the hop on this host: the generated
// transport checks the connection the dialer made, and refuses it, naming why,
// when its peer is not loopback. Each case also sets its own transport
// credentials, which must not displace the generated ones.
func TestGatewayHopRefusesADialerThatLeavesLoopback(t *testing.T) {
	t.Run("interface address", func(t *testing.T) {
		// The generated gRPC server listens on every interface, so a dialer
		// pointed at this host's own non-loopback address reaches the very
		// server the hop is for: without the check, every call here succeeds.
		address := nonLoopbackAddress(t)
		config := &Configuration{}
		config.GatewayDialOptions = []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(int(config.EndpointGrpcPort))))
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		}
		base, client := startConfiguredService(t, config)

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		_, err := genconnect.NewWebServiceClient(client, base).Version(ctx, connect.NewRequest(&gen.VersionRequest{}))
		if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), errGatewayHopOffLoopback.Error()) {
			t.Fatalf("Connect call over a hop to %s: %v, want unavailable naming the loopback rule", address, err)
		}

		restBase := fmt.Sprintf("http://127.0.0.1:%d", *config.EndpointHttpPort)
		waitForHTTP(t, restBase+"/healthz")
		served := restGet(t, restBase+"/version", nil)
		if body := restBody(t, served); served.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, errGatewayHopOffLoopback.Error()) {
			t.Fatalf("REST call over a hop to %s: status %d: %s", address, served.StatusCode, body)
		}
		if health := restGet(t, restBase+"/healthz", nil); health.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("health probe over a hop to %s: status %d, want 503", address, health.StatusCode)
		}
	})

	t.Run("reported peer", func(t *testing.T) {
		// Whatever a dialer did, the connection's peer is all the hop can see:
		// one that reports a remote peer is refused before the hop writes any
		// of the call, which the listener behind it proves by receiving
		// nothing. This case needs no interface besides loopback.
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		var received atomic.Int64
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				n, _ := io.Copy(io.Discard, conn)
				received.Add(n)
			}
		}()
		remote := &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 443}
		options := gatewayDialOptions(
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
				if err != nil {
					return nil, err
				}
				return reportedPeer{Conn: conn, remote: remote}, nil
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", listener.Addr().(*net.TCPAddr).Port), options...)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_, err = gen.NewWebServiceClient(conn).Version(ctx, &gen.VersionRequest{})
		_ = conn.Close()
		_ = listener.Close()
		<-done
		if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), errGatewayHopOffLoopback.Error()) || !strings.Contains(err.Error(), remote.String()) {
			t.Fatalf("hop to a reported %s: %v, want unavailable naming the loopback rule and the peer", remote, err)
		}
		if got := received.Load(); got != 0 {
			t.Fatalf("the refused hop wrote %d bytes to its peer", got)
		}
	})
}

// isLoopback decides the hop, so its edges are pinned: both families'
// loopback ranges pass, an IPv4 loopback address in IPv6 form passes, and
// everything else, an address with no IP among it, fails.
func TestGatewayHopLoopbackAddresses(t *testing.T) {
	for address, want := range map[string]bool{
		"127.0.0.1:8080":        true,
		"127.10.0.1:8080":       true,
		"[::1]:8080":            true,
		"[::ffff:127.0.0.1]:80": true,
		"0.0.0.0:8080":          false,
		"[::]:8080":             false,
		"192.0.2.10:443":        false,
		"[2001:db8::1]:443":     false,
		"localhost:8080":        false,
		"bufconn":               false,
	} {
		if got := isLoopback(stringAddr(address)); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", address, got, want)
		}
	}
	if isLoopback(nil) {
		t.Error("isLoopback(nil) = true")
	}
}

type reportedPeer struct {
	net.Conn
	remote net.Addr
}

func (c reportedPeer) RemoteAddr() net.Addr { return c.remote }

type stringAddr string

func (a stringAddr) Network() string { return "tcp" }
func (a stringAddr) String() string  { return string(a) }

// nonLoopbackAddress is an IPv4 address of this host that is not loopback, the
// address a hop leaving the host would reach.
func nonLoopbackAddress(t *testing.T) string {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		network, ok := address.(*net.IPNet)
		if !ok || network.IP.To4() == nil || network.IP.IsLoopback() || network.IP.IsLinkLocalUnicast() {
			continue
		}
		return network.IP.String()
	}
	t.Skip("this host has no non-loopback IPv4 address to dial")
	return ""
}
