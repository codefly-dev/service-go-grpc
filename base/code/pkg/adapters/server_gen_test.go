package adapters

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServerStopReleasesHTTPListeners(t *testing.T) {
	ports := unusedPorts(t, 3)
	config := &Configuration{
		EndpointGrpcPort:    ports[0],
		EndpointHttpPort:    portPointer(ports[1]),
		EndpointConnectPort: portPointer(ports[2]),
	}

	for run := 0; run < 2; run++ {
		server, err := NewServer(config)
		if err != nil {
			t.Fatalf("create server for run %d: %v", run, err)
		}

		started := make(chan error, 1)
		go func() {
			started <- server.Start(context.Background())
		}()

		waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", *config.EndpointHttpPort))
		waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/", *config.EndpointConnectPort))
		server.Stop()

		select {
		case err := <-started:
			if err != nil {
				t.Fatalf("stop server for run %d: %v", run, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("server run %d did not stop", run)
		}
	}
}

func unusedPorts(t *testing.T, count int) []uint16 {
	t.Helper()
	listeners := make([]net.Listener, 0, count)
	for range count {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("allocate port: %v", err)
		}
		listeners = append(listeners, listener)
	}
	ports := make([]uint16, 0, count)
	for _, listener := range listeners {
		ports = append(ports, uint16(listener.Addr().(*net.TCPAddr).Port))
		if err := listener.Close(); err != nil {
			t.Fatalf("release port: %v", err)
		}
	}
	return ports
}

func portPointer(port uint16) *uint16 {
	return &port
}

func waitForHTTP(t *testing.T, url string) {
	t.Helper()
	client := &http.Client{Timeout: 250 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err == nil {
			response.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("HTTP server did not start at %s", url)
}
