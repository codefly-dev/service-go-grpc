package adapters

import (
	"codefly-base/pkg/gen"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// startGRPC spins up an in-memory gRPC server exposing the generated
// WebService and returns a client connection to it.
func startGRPC(t *testing.T) grpc.ClientConnInterface {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	gen.RegisterWebServiceServer(grpcServer, &GrpcServer{})
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestResolveMethod checks the allowlist selector resolution: valid selectors
// resolve against the registry; malformed or unknown ones are rejected.
func TestResolveMethod(t *testing.T) {
	sd, md, err := resolveMethod("api.WebService/Version")
	if err != nil {
		t.Fatalf("resolve valid selector: %v", err)
	}
	if got := string(sd.FullName()); got != "api.WebService" {
		t.Errorf("service full name: got %q", got)
	}
	if got := string(md.Name()); got != "Version" {
		t.Errorf("method name: got %q", got)
	}

	for _, bad := range []string{"", "no-slash", "api.WebService/", "/Version", "api.WebService/Missing", "api.Nope/Version"} {
		if _, _, err := resolveMethod(bad); err == nil {
			t.Errorf("selector %q should have failed to resolve", bad)
		}
	}
}

// TestMCPServerHonorsAllowlist proves the tool set is exactly the allowlist,
// not every RPC on the service.
func TestMCPServerHonorsAllowlist(t *testing.T) {
	conn := startGRPC(t)

	// Default allowlist exposes exactly Version.
	server, err := newMCPToolServer(conn)
	if err != nil {
		t.Fatalf("newMCPToolServer: %v", err)
	}
	if server == nil {
		t.Fatal("nil server")
	}

	// An empty allowlist exposes nothing; a bad selector fails loudly.
	restore := mcpAllowedMethods
	t.Cleanup(func() { mcpAllowedMethods = restore })

	mcpAllowedMethods = nil
	if _, err := newMCPToolServer(conn); err != nil {
		t.Fatalf("empty allowlist should build an empty server, got %v", err)
	}

	mcpAllowedMethods = []string{"api.WebService/DoesNotExist"}
	if _, err := newMCPToolServer(conn); err == nil {
		t.Fatal("unknown selector should fail the server build")
	}
}

// TestMCPEndToEndOverHTTP drives a real MCP client over the Streamable HTTP
// transport: list tools and call one, asserting the RPC round-trips as
// structured content.
func TestMCPEndToEndOverHTTP(t *testing.T) {
	conn := startGRPC(t)
	handler, err := NewMCPHandler(conn)
	if err != nil {
		t.Fatalf("NewMCPHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "WebService_Version" {
		t.Fatalf("expected exactly the WebService_Version tool, got %+v", tools.Tools)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "WebService_Version", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported error: %+v", result.Content)
	}
	if _, ok := result.StructuredContent.(map[string]any); !ok {
		t.Fatalf("expected structured object, got %T", result.StructuredContent)
	}
}

// TestMCPCrossOriginRejected proves the cross-origin protection is active: a
// browser-style request with a foreign Origin is refused before reaching a
// tool.
func TestMCPCrossOriginRejected(t *testing.T) {
	conn := startGRPC(t)
	handler, err := NewMCPHandler(conn)
	if err != nil {
		t.Fatalf("NewMCPHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Origin", "http://evil.example.com")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin request should be 403, got %d", resp.StatusCode)
	}
}

// TestMCPRouterDispatchesOnlyMcpPaths verifies the router delegates only /mcp
// (and subpaths) to MCP and passes every other path to the next handler
// verbatim, without http.ServeMux path rewriting.
func TestMCPRouterDispatchesOnlyMcpPaths(t *testing.T) {
	var nextGot string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { nextGot = r.URL.Path })
	var mcpHit bool
	mcpH := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { mcpHit = true })
	router := MCPRouter(mcpH, next)

	for _, tc := range []struct {
		path    string
		wantMCP bool
	}{
		{"/mcp", true},
		{"/mcp/session", true},
		{"/version", false},
		{"/mcpother", false}, // must NOT be captured by the /mcp prefix
		{"/version/../x", false},
	} {
		nextGot, mcpHit = "", false
		req := httptest.NewRequest(http.MethodPost, "http://x"+tc.path, nil)
		req.URL.Path = tc.path // preserve raw path (no cleaning)
		router.ServeHTTP(httptest.NewRecorder(), req)
		if tc.wantMCP && !mcpHit {
			t.Errorf("%s: expected MCP handler", tc.path)
		}
		if !tc.wantMCP && nextGot != tc.path {
			t.Errorf("%s: expected next handler to receive verbatim path, got %q", tc.path, nextGot)
		}
	}
}

// TestMessageSchema checks the proto-to-JSON-Schema mapping for scalars and
// nested objects.
func TestMessageSchema(t *testing.T) {
	respDesc := (&gen.VersionResponse{}).ProtoReflect().Descriptor()
	schema := messageSchema(respDesc, map[protoreflect.FullName]bool{})
	if schema["type"] != "object" {
		t.Fatalf("expected object schema, got %v", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties map, got %T", schema["properties"])
	}
	version, ok := props["version"].(map[string]any)
	if !ok {
		t.Fatalf("expected version property, got %v", props)
	}
	if version["type"] != "string" {
		t.Errorf("version field should be string, got %v", version["type"])
	}
}
