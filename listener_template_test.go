package main

import (
	"strings"
	"testing"
)

// TestGeneratedFacesServeOnAListenerAndReturnTheirFailures pins the shape that
// ends a port race in every service this agent generates.
//
// The faces used to bind by port number inside Run (net.Listen in the gRPC
// one, ListenAndServe in the REST and Connect ones) and took no listener, so a
// test starting a real server had to find a free port, release it and pass the
// number on, and lost it to whatever bound in between. The startup file then
// ran REST and Connect in goroutines that panicked on any error, so that lost
// bind killed the test binary where no caller could catch or retry it.
//
// Rendered for every listener setting, because the REST plumbing is behind a
// template conditional: a service without REST must not be handed an
// HttpListener field that nothing would serve. The behaviour itself is held by
// base/code/pkg/adapters/listener_test.go, which
// TestBaseGeneratedServiceServesConfiguredRPCsOverConnect runs.
func TestGeneratedFacesServeOnAListenerAndReturnTheirFailures(t *testing.T) {
	for name, settings := range map[string]*Settings{
		"REST and Connect": {RestEndpoint: true, ConnectEndpoint: true},
		"Connect":          {ConnectEndpoint: true},
		"gRPC":             {},
	} {
		t.Run(name, func(t *testing.T) {
			render := func(file string) string {
				return string(renderFactoryGo(t, "templates/factory/code/pkg/adapters/"+file+".tmpl", settings))
			}
			grpc, server, connect, rest := render("grpc_gen.go"), render("server_gen.go"), render("connect_gen.go"), render("rest_gen.go")

			for _, want := range []string{"GrpcListener", "ConnectListener", "func listenOn(supplied net.Listener,", "s.gRPC.Serve(s.listener)"} {
				if !strings.Contains(grpc, want) {
					t.Errorf("gRPC adapter does not contain %q", want)
				}
			}
			if !strings.Contains(connect, "s.server.Serve(s.listener)") {
				t.Error("Connect adapter does not serve on the listener it holds")
			}
			if settings.RestEndpoint && !strings.Contains(rest, "s.server.Serve(s.listener)") {
				t.Error("REST adapter does not serve on the listener it holds")
			}
			for file, rendered := range map[string]string{"grpc_gen.go": grpc, "server_gen.go": server, "connect_gen.go": connect, "rest_gen.go": rest} {
				if strings.Contains(rendered, "ListenAndServe") {
					t.Errorf("%s binds by address inside the serve call, where no listener can be supplied", file)
				}
			}
			// The one net.Listen is listenOn's: every face binds through it.
			if got := strings.Count(grpc+server+connect+rest, "net.Listen("); got != 1 {
				t.Errorf("the adapters call net.Listen %d times, want the one in listenOn", got)
			}
			if strings.Contains(server, "panic(") {
				t.Errorf("the startup file panics instead of returning a failure to Start's caller:\n%s", server)
			}
			for _, want := range []string{"func (server *Server) Listen() error", "func (server *Server) GrpcAddr() net.Addr", "func (server *Server) ConnectAddr() net.Addr", "server.release()"} {
				if !strings.Contains(server, want) {
					t.Errorf("startup file does not contain %q", want)
				}
			}
			for file, rendered := range map[string]string{"grpc_gen.go": grpc, "server_gen.go": server} {
				if got := strings.Contains(rendered, "HttpListener"); got != settings.RestEndpoint {
					t.Errorf("%s mentions HttpListener: %v, REST endpoint: %v", file, got, settings.RestEndpoint)
				}
			}
			if got := strings.Contains(server, "func (server *Server) HttpAddr() net.Addr"); got != settings.RestEndpoint {
				t.Errorf("startup file has HttpAddr: %v, REST endpoint: %v", got, settings.RestEndpoint)
			}
		})
	}
}
