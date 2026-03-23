package main

import (
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
)

func goGrpcTechniques() []*agentv0.AgentTechnique {
	return []*agentv0.AgentTechnique{
		{
			Id:          "go-grpc-file-layout",
			Name:        "Go gRPC File Layout",
			Description: "Which files are user-editable vs auto-generated in a go-grpc service",
			Tags:        []string{"go-grpc", "layout", "generated"},
			Prompt: `GO-GRPC FILE LAYOUT:
Editable files (YOUR code):
  pkg/adapters/rpcs.go     — RPC method implementations
  pkg/business/**           — Business logic, domain types, helpers
  proto/api.proto           — gRPC service and message definitions

Auto-generated files (NEVER edit manually):
  pkg/gen/*.pb.go           — Protobuf message Go code
  pkg/gen/*_grpc.pb.go      — gRPC client/server stubs
  pkg/gen/*.pb.gw.go        — REST gateway reverse proxy
  pkg/adapters/*_gen.go     — Adapter wiring (server registration, CORS, REST)
  pkg/adapters/grpc_gen.go  — gRPC server constructor
  pkg/adapters/rest_gen.go  — REST gateway constructor
  pkg/adapters/server_gen.go— Unified server startup
  pkg/adapters/cors_gen.go  — CORS middleware
  go.sum                    — Dependency lock file

If you need to change generated files, modify the source (proto or templates) and regenerate.`,
		},
		{
			Id:          "go-grpc-proto-flow",
			Name:        "Go gRPC Proto-to-Code Flow",
			Description: "How proto definitions flow through code generation to runtime code",
			Tags:        []string{"go-grpc", "proto", "codegen"},
			Prompt: `GO-GRPC PROTO FLOW:
1. Define your service in proto/api.proto (messages, RPCs, HTTP annotations).
2. Run codefly to regenerate: proto/api.proto → buf generate → pkg/gen/*.pb.go
3. The plugin auto-generates adapter wiring in pkg/adapters/*_gen.go.
4. YOU implement RPC handlers in pkg/adapters/rpcs.go, calling business logic in pkg/business/.

Adding a new RPC:
  a) Add the rpc definition in proto/api.proto
  b) Add HTTP annotation if REST is enabled
  c) Regenerate (codefly handles this)
  d) Implement the handler method in pkg/adapters/rpcs.go
  e) Add domain logic in pkg/business/`,
		},
		{
			Id:          "go-grpc-rpc-authoring",
			Name:        "Go gRPC RPC Implementation",
			Description: "How to write RPC method implementations in rpcs.go",
			Tags:        []string{"go-grpc", "rpc", "implementation"},
			Prompt: `GO-GRPC RPC AUTHORING:
Write RPC implementations in pkg/adapters/rpcs.go.
Each method signature matches the proto service definition.
Example:

  func (s *GrpcServer) MyMethod(ctx context.Context, req *gen.MyMethodRequest) (*gen.MyMethodResponse, error) {
      // Call business logic
      result, err := s.businessLogic.DoSomething(ctx, req.Input)
      if err != nil {
          return nil, status.Errorf(codes.Internal, "failed: %v", err)
      }
      return &gen.MyMethodResponse{Output: result}, nil
  }

Rules:
- Return proper gRPC status codes (NotFound, InvalidArgument, Internal, etc.)
- Keep handlers thin — delegate to pkg/business/ for domain logic
- Use the generated request/response types from pkg/gen/
- Access injected dependencies through the server struct`,
		},
		{
			Id:          "go-grpc-infra-pattern",
			Name:        "Go gRPC Infrastructure",
			Description: "Infrastructure and dependency patterns for go-grpc services",
			Tags:        []string{"go-grpc", "infra", "dependencies"},
			Prompt: `GO-GRPC INFRASTRUCTURE:
Infrastructure code lives in pkg/infra/.
- Database connections, external API clients, caches go here.
- Wire them into main.go and pass to business logic constructors.
- main.go is the composition root: it creates infra, creates business, creates adapters, starts server.
- The server auto-handles health checks, graceful shutdown, and signal handling.
- Environment variables and service endpoints are injected by codefly at runtime.`,
		},
	}
}
