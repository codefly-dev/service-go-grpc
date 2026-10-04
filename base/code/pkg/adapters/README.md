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
||||||| e58f844
## Extending the REST listener

`rest_gen.go` is generated: a sync overwrites it, so an edit to it disappears
with no build error. Everything the REST listener lets a service change is a
field on `Configuration` (in `grpc_gen.go`), set from the `Configure` function
registered with `WithConfigure`, and applied by the generated `Run`. The two
fields that are not REST-specific — `Service`, the implementation that answers
every RPC, and `GRPCServerOptions`, the transport policy installed on the served
gRPC server — are documented with the gRPC listener.

| Field | What it does |
| --- | --- |
| `ServeMuxOptions []runtime.ServeMuxOption` | Appended to the gateway mux's generated options. The only way to install a `runtime.WithForwardResponseOption` (a strong `ETag`, a `Cache-Control`, a `304` on one RPC), `WithIncomingHeaderMatcher`, `WithOutgoingHeaderMatcher` or `WithMarshalerOption`: options reach a mux at construction, and the `plugins.RegisterREST` seam receives it already built. |
| `Routes []Route` | HTTP handlers served ahead of the gateway, matched by plain path prefix. For a protocol the gateway cannot carry — a Connect handler for a second protobuf service, whose every method would otherwise need its own `gwMux.HandlePath` template. |
| `Middleware []func(http.Handler) http.Handler` | Wraps the chain, the first entry outermost, inside the CORS policy and ahead of the routes and the gateway. For policy that must see a request before anything reads its body: a request-body bound, a tracing span, a rate limit. |
| `GatewayDialOptions []grpc.DialOption` | Appended to the options this listener dials the gRPC server with, for the gateway's hop and the health probe's client. Where a service raises gRPC's 4 MiB default call bounds — `grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(n), grpc.MaxCallSendMsgSize(n))` — since a bound set only on the server leaves the proxy refusing a message the server would have accepted. |

The handler chain, outermost first: the generated request logging, the CORS
policy from your `cors:` settings, your `Middleware`, your `Routes`, the gateway.
`gatewayMuxOptions(extra ...runtime.ServeMuxOption)`,
`gatewayDialOptions(extra ...grpc.DialOption)`, `WithRoutes` and `WithMiddleware`
are named so your own tests build the same mux and the same chain the server
runs, rather than a copy that drifts from it:
`runtime.NewServeMux(gatewayMuxOptions(config.ServeMuxOptions...)...)`.

### Two rules that are easy to get wrong

**Option ordering.** Your `ServeMuxOptions` are applied *after* the generated
ones, so an option that sets a single value (`WithErrorHandler`, either header
matcher, `WithMarshalerOption` for an already-registered MIME type) **replaces**
the generated default — a replaced error handler drops the generated "Route not
found" 404 mapping, so re-implement it — while an option that accumulates
(`WithMetadata`, `WithForwardResponseOption`) runs **in addition**, after it.
Which is why a header the generated annotator already forwards must not also be
matched in a `WithIncomingHeaderMatcher`: grpc-gateway joins the matcher's pairs
with each annotator's metadata, and two identical values is what an ambiguity
check refuses.

**What a mounted route gets.** `Routes` match by prefix, first match wins, and a
route is consulted before every gateway path including `/healthz` — so do not
mount `"/"`. The listener speaks HTTP/1.1, which serves a Connect or gRPC-Web
caller but not a gRPC-over-h2c one; that is the Connect listener
`connect-endpoint` declares.

### What the listener logs

A non-200 response is recorded with its method, path, status and declared body
size, and nothing else. The request body is deliberately absent: the handler
this replaced read every body into memory so it could log the payload on any
non-200, which copied the caller's data into a log store with its own retention
and audience, on exactly the paths where something had already gone wrong — and
the buffering kept any handler here from being reached before the caller sent
EOF. A service that wants more than the outcome adds its own `Middleware`.
