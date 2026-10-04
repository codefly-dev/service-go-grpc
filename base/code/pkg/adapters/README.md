# Hexagonal Architecture

## Adapters

### gRPC

We want to leave the adapters scope as soon as possible to spend as much time as possible in the core package.

Write your RPCs handlers in `rpcs.go`.

### REST

Auto-generated code from protobuf definitions.

## The configuration contract

`Configuration` (in `grpc_gen.go`) is how a service wires itself into the
generated listeners. Fill it in a `Configure` function registered with
`WithConfigure` in your own file beside `main.go`; it runs after codefly has
injected the service's capabilities and before any listener starts.

| Field | What it does |
| --- | --- |
| `Service` | The implementation that answers every RPC. Set it to your own type — built with its real dependencies — and the generated Version-only implementation steps aside. |
| `GRPCServerOptions` | `grpc.ServerOption`s installed on the served gRPC server: authentication and authorization interceptors, telemetry, rate limiting, message bounds such as `grpc.MaxRecvMsgSize`. |
| `EndpointGrpcPort`, `EndpointHttpPort`, `EndpointConnectPort` | Resolved by codefly from the endpoints the service declares; `main.go` fills them. |

One implementation, one policy chain, three listeners:

- **gRPC** (`grpc_gen.go`) serves `Service` directly, with `GRPCServerOptions`
  applied.
- **Connect** (`connect_gen.go`) serves the Connect, gRPC and gRPC-Web protocols
  on its own port. It transcodes each request to gRPC and dispatches it through
  that same gRPC server, so `Service` answers a Connect call exactly as it
  answers a gRPC one, every `GRPCServerOptions` entry runs first, and the gRPC
  status a handler returns — error details included — arrives as the equivalent
  Connect error. There is no Connect-shaped copy of your API to implement, and a
  method added to the proto is served over Connect as soon as it is served over
  gRPC. Request headers arrive as gRPC metadata, so an interceptor reading
  `metadata.FromIncomingContext` sees them on either listener.
- **REST** (`rest_gen.go`) proxies to the gRPC listener through grpc-gateway,
  wrapped in the CORS policy from your `cors:` settings. The `google.api.http`
  annotation paths belong to this listener alone; the Connect port answers the
  RPC protocols only.

Each listener is started only when its endpoint is declared and scaffolded
(`rest-endpoint` / `connect-endpoint` in the service's settings), and the three
are independent: enabling Connect changes nothing about how gRPC or REST start.
