package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"codefly-base/pkg/gen"
	"codefly-base/pkg/gen/genconnect"
	"connectrpc.com/connect"
)

// Run the generated entrypoint, not a hand-assembled Configuration: otherwise
// losing the host between endpoint resolution and the adapter goes unnoticed.
func TestListenerProcess(t *testing.T) {
	if os.Getenv("TEST_LISTENER_PROCESS") == "1" {
		main()
	}
}

func TestGeneratedListenersUseOwnHosts(t *testing.T) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var offLoopback string
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			offLoopback = ip.String()
			break
		}
	}
	if offLoopback == "" {
		t.Skip("non-loopback interface required to distinguish wildcard binding")
	}
	for _, tc := range []struct {
		name, runtime, host string
		wildcard            bool
	}{
		{"native-v4", "native", "127.0.0.1", false},
		{"native-v6-url-carrier", "native", "::1", false},
		{"native-localhost", "native", "localhost", false},
		{"nix", "nix", "127.0.0.1", false},
		{"container", "container", "localhost", true},
		{"kubernetes", "kubernetes", "localhost", true},
		{"legacy-absent-context", "", "localhost", true},
		{"blank-context", " ", "localhost", true},
		{"unknown-follows-core-native-default", "unknown", "127.0.0.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ports := make([]string, 3)
			for i := range ports {
				listener, err := net.Listen("tcp", net.JoinHostPort(tc.host, "0"))
				if err != nil {
					t.Fatal(err)
				}
				_, ports[i], _ = net.SplitHostPort(listener.Addr().String())
				listener.Close()
			}
			// Reuse exactly the same ports after a clean stop, as a reload does.
			for iteration := 0; iteration < 2; iteration++ {
				func() {
					command := exec.Command(os.Args[0], "-test.run=^TestListenerProcess$")
					for _, env := range os.Environ() {
						if !strings.HasPrefix(env, "CODEFLY__") {
							command.Env = append(command.Env, env)
						}
					}
					command.Env = append(command.Env, "TEST_LISTENER_PROCESS=1", "CODEFLY__MODULE=test", "CODEFLY__SERVICE=listener", "CODEFLY__SERVICE_VERSION=listener-proof")
					if tc.runtime != "" {
						command.Env = append(command.Env, "CODEFLY__RUNTIME_CONTEXT="+tc.runtime)
					}
					for i, api := range []string{"GRPC", "REST", "CONNECT"} {
						// The rendered Connect-only fixture declares a nonconventional name.
						name := api
						if os.Getenv("TEST_CONNECT_ONLY") == "1" && i == 2 {
							name = "FACE2"
						}
						address := net.JoinHostPort(tc.host, ports[i])
						// Core accepts IPv6 URL carriers; its bare IPv6 parser is a
						// separate limitation. Exercise the delivered Hostname here.
						if api != "GRPC" || tc.host == "::1" {
							address = "http://" + address
						}
						command.Env = append(command.Env, fmt.Sprintf("CODEFLY__ENDPOINT__TEST__LISTENER__%s__%s=%s", name, api, address))
					}
					log, err := os.Create(filepath.Join(t.TempDir(), "listener.log"))
					if err != nil {
						t.Fatal(err)
					}
					defer log.Close()
					command.Stdout, command.Stderr = log, log
					if err := command.Start(); err != nil {
						t.Fatal(err)
					}
					defer func() {
						command.Process.Signal(syscall.SIGTERM)
						done := make(chan error, 1)
						go func() { done <- command.Wait() }()
						select {
						case err := <-done:
							if err != nil {
								t.Errorf("listener exit: %v", err)
							}
						case <-time.After(5 * time.Second):
							command.Process.Kill()
							<-done
							t.Error("listener did not stop")
						}
						if t.Failed() {
							data, _ := os.ReadFile(log.Name())
							t.Log(string(data))
						}
					}()
					client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
					defer client.CloseIdleConnections()
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					c := genconnect.NewWebServiceClient(client, "http://"+net.JoinHostPort(tc.host, ports[2]))
					for {
						answer, err := c.Version(ctx, connect.NewRequest(&gen.VersionRequest{}))
						if err == nil && answer.Msg.Version == "listener-proof" {
							break
						}
						if ctx.Err() != nil {
							t.Fatalf("generated listener not ready: %v", err)
						}
						time.Sleep(20 * time.Millisecond)
					}
					if os.Getenv("TEST_CONNECT_ONLY") != "1" {
						response, err := client.Get("http://" + net.JoinHostPort(tc.host, ports[1]) + "/healthz")
						if err != nil {
							t.Fatal(err)
						}
						response.Body.Close()
						if response.StatusCode != 200 {
							t.Fatalf("REST self-hop health: %s", response.Status)
						}
					}
					for i, port := range ports {
						if os.Getenv("TEST_CONNECT_ONLY") == "1" && i != 2 {
							continue
						}
						connection, err := net.DialTimeout("tcp", net.JoinHostPort(offLoopback, port), time.Second)
						if connection != nil {
							connection.Close()
						}
						if (err == nil) != tc.wildcard {
							t.Fatalf("port %s off-loopback reachability: err=%v, want wildcard=%v", port, err, tc.wildcard)
						}
					}
				}()
			}
		})
	}
}

func TestNativeListenerRefusesMissingHost(t *testing.T) {
	t.Setenv("CODEFLY__RUNTIME_CONTEXT", "native")
	defer func() {
		if recover() == nil {
			t.Fatal("missing native hostname must not become wildcard binding")
		}
	}()
	listenerHost("")
}
