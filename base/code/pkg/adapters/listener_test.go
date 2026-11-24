package adapters

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// TestServersOnSuppliedListenersNeverCollide holds the reason the faces take a
// listener. A test that starts a real server used to pick free ports, release
// them, and hand the numbers over, and whatever bound in between (a sibling
// test binary, most often) took one: `bind: address already in use`, one run
// in four in a consumer's suite. A listener bound on 127.0.0.1:0 is held from
// the moment the kernel picks its port, so servers started side by side cannot
// take each other's, and each answers on its own three addresses. The REST
// probe also proves the hop between faces follows the supplied gRPC listener
// rather than a port the Configuration never had.
func TestServersOnSuppliedListenersNeverCollide(t *testing.T) {
	const servers = 2
	failures := make(chan error, servers)
	var running sync.WaitGroup
	for range servers {
		running.Add(1)
		go func() {
			defer running.Done()
			failures <- serveOnceOnSuppliedListeners(t)
		}()
	}
	running.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Error(err)
		}
	}
}

// serveOnceOnSuppliedListeners starts a server on three listeners it bound,
// reaches every face at its listener's address, and stops it.
func serveOnceOnSuppliedListeners(t *testing.T) error {
	config := &Configuration{
		GrpcListener:    loopbackListener(t),
		HttpListener:    loopbackListener(t),
		ConnectListener: loopbackListener(t),
	}
	server, err := NewServer(config)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}
	started := make(chan error, 1)
	go func() { started <- server.Start(context.Background()) }()
	defer server.Stop()

	// No readiness poll: the listeners are bound already, so a connection made
	// before the server accepts waits in the backlog instead of being refused.
	for face, address := range map[string]net.Addr{"gRPC": config.GrpcListener.Addr(), "Connect": config.ConnectListener.Addr()} {
		if err := checkHealthOverGRPC(address.String()); err != nil {
			return fmt.Errorf("%s face at %s: %w", face, address, err)
		}
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(fmt.Sprintf("http://%s/healthz", config.HttpListener.Addr()))
	if err != nil {
		return fmt.Errorf("REST face at %s: %w", config.HttpListener.Addr(), err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("REST face at %s answered %d: %s", config.HttpListener.Addr(), response.StatusCode, body)
	}
	for face, pair := range map[string][2]net.Addr{
		"gRPC":    {server.GrpcAddr(), config.GrpcListener.Addr()},
		"REST":    {server.HttpAddr(), config.HttpListener.Addr()},
		"Connect": {server.ConnectAddr(), config.ConnectListener.Addr()},
	} {
		if pair[0] == nil || pair[0].String() != pair[1].String() {
			return fmt.Errorf("%s face reports %v, bound on %v", face, pair[0], pair[1])
		}
	}

	server.Stop()
	select {
	case err := <-started:
		if err != nil {
			return fmt.Errorf("Start after Stop: %w", err)
		}
	case <-time.After(10 * time.Second):
		return errors.New("Start did not return after Stop")
	}
	return nil
}

// TestBindFailureIsReturnedToTheCaller holds the other half of the defect: the
// REST and Connect faces were run in goroutines that panicked on any error, so
// a port already taken killed the whole test binary from a goroutine no caller
// could recover or retry. Each face in turn is pointed at a port that is held:
// Start returns that bind failure, before anything serves, and closes the
// listeners it was handed for the other faces rather than leaking them.
func TestBindFailureIsReturnedToTheCaller(t *testing.T) {
	for _, face := range []string{"gRPC", "REST", "Connect"} {
		t.Run(face, func(t *testing.T) {
			held := loopbackListener(t)
			host, taken := held.Addr().(*net.TCPAddr).IP.String(), uint16(held.Addr().(*net.TCPAddr).Port)
			config := &Configuration{}
			var supplied []net.Listener
			supply := func() net.Listener {
				listener := loopbackListener(t)
				supplied = append(supplied, listener)
				return listener
			}
			if face == "gRPC" {
				config.EndpointGrpcHost, config.EndpointGrpcPort = host, taken
			} else {
				config.GrpcListener = supply()
			}
			if face == "REST" {
				config.EndpointHttpHost, config.EndpointHttpPort = host, &taken
			} else {
				config.HttpListener = supply()
			}
			if face == "Connect" {
				config.EndpointConnectHost, config.EndpointConnectPort = host, &taken
			} else {
				config.ConnectListener = supply()
			}
			server, err := NewServer(config)
			if err != nil {
				t.Fatalf("create server: %v", err)
			}
			t.Cleanup(server.Stop)

			started := make(chan error, 1)
			go func() { started <- server.Start(context.Background()) }()
			select {
			case err := <-started:
				if !errors.Is(err, syscall.EADDRINUSE) {
					t.Fatalf("Start on a held port returned %v, want the bind failure", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Start on a held port neither failed nor returned")
			}
			for _, listener := range supplied {
				if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
					t.Errorf("a failed start left the listener at %s open: Accept returned %v", listener.Addr(), err)
				}
			}
			for name, address := range map[string]net.Addr{"gRPC": server.GrpcAddr(), "REST": server.HttpAddr(), "Connect": server.ConnectAddr()} {
				if address != nil {
					t.Errorf("a failed start still reports the %s face bound at %s", name, address)
				}
			}
		})
	}
}

// TestServeFailureIsReturnedToTheCaller covers a failure after the bind: a
// face that cannot serve on its listener. It used to be a panic in a goroutine
// for REST and Connect; Start returns it and stops the faces that did start.
func TestServeFailureIsReturnedToTheCaller(t *testing.T) {
	for _, face := range []string{"gRPC", "REST", "Connect"} {
		t.Run(face, func(t *testing.T) {
			config := &Configuration{
				GrpcListener:    loopbackListener(t),
				HttpListener:    loopbackListener(t),
				ConnectListener: loopbackListener(t),
			}
			broken := map[string]net.Listener{"gRPC": config.GrpcListener, "REST": config.HttpListener, "Connect": config.ConnectListener}[face]
			if err := broken.Close(); err != nil {
				t.Fatalf("close the %s listener: %v", face, err)
			}
			server, err := NewServer(config)
			if err != nil {
				t.Fatalf("create server: %v", err)
			}
			t.Cleanup(server.Stop)

			started := make(chan error, 1)
			go func() { started <- server.Start(context.Background()) }()
			select {
			case err := <-started:
				if !errors.Is(err, net.ErrClosed) {
					t.Fatalf("Start on a closed %s listener returned %v, want the serve failure", face, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("Start on a closed %s listener neither failed nor returned", face)
			}
		})
	}
}

// TestStopReleasesEveryListener holds graceful shutdown on the listener path:
// after Stop the three addresses can be bound again, whether the server served
// on them or was stopped having only taken them (Listen without Start), which
// no face's own shutdown would have closed.
func TestStopReleasesEveryListener(t *testing.T) {
	for _, serve := range []bool{true, false} {
		name := "served"
		if !serve {
			name = "listening only"
		}
		t.Run(name, func(t *testing.T) {
			config := &Configuration{
				GrpcListener:    loopbackListener(t),
				HttpListener:    loopbackListener(t),
				ConnectListener: loopbackListener(t),
			}
			server, err := NewServer(config)
			if err != nil {
				t.Fatalf("create server: %v", err)
			}
			if err := server.Listen(); err != nil {
				t.Fatalf("listen: %v", err)
			}
			addresses := []net.Addr{server.GrpcAddr(), server.HttpAddr(), server.ConnectAddr()}
			started := make(chan error, 1)
			if serve {
				go func() { started <- server.Start(context.Background()) }()
				if err := checkHealthOverGRPC(server.ConnectAddr().String()); err != nil {
					t.Fatalf("the server did not serve: %v", err)
				}
			}
			server.Stop()
			if serve {
				select {
				case err := <-started:
					if err != nil {
						t.Fatalf("Start after Stop: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("Start did not return after Stop")
				}
			}
			for _, address := range addresses {
				again, err := net.Listen("tcp", address.String())
				if err != nil {
					t.Errorf("Stop left %s bound: %v", address, err)
					continue
				}
				again.Close()
			}
		})
	}
}

// TestListenReportsTheAddressBoundByPort keeps the by-port entry point on the
// same path: a Connect endpoint declared on port zero is bound by Listen, and
// the port the kernel chose is readable before the server serves, where it
// used to be known to nobody.
func TestListenReportsTheAddressBoundByPort(t *testing.T) {
	var any uint16
	config := &Configuration{EndpointConnectHost: net.IPv4(127, 0, 0, 1).String(), EndpointConnectPort: &any}
	server, err := NewServer(config)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	if address := server.ConnectAddr(); address != nil {
		t.Fatalf("the Connect face reports %s before it listens", address)
	}
	if err := server.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(server.Stop)
	bound, ok := server.ConnectAddr().(*net.TCPAddr)
	if !ok || bound.Port == 0 {
		t.Fatalf("the Connect face reports %v, want the bound loopback port", server.ConnectAddr())
	}
	started := make(chan error, 1)
	go func() { started <- server.Start(context.Background()) }()
	if err := checkHealthOverGRPC(bound.String()); err != nil {
		t.Fatalf("the server does not answer at the address it reports: %v", err)
	}
	server.Stop()
	if err := <-started; err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
}

func loopbackListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

// checkHealthOverGRPC asks the gRPC health service at address, which both the
// gRPC face and the Connect face serve.
func checkHealthOverGRPC(address string) error {
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{}, grpc.WaitForReady(true))
	if err != nil {
		return err
	}
	if response.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		return fmt.Errorf("health is %s", response.GetStatus())
	}
	return nil
}
