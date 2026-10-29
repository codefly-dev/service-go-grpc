# Service protocol metadata

`templates/deployment/kustomize/base/service.yaml.tmpl` renders one Service
port per listener the process binds. Each port declares the protocol its
listener actually speaks. An L7 hop in front of the pod (an Istio waypoint, an
ingress) chooses the upstream protocol from that declaration: `appProtocol`
when set, otherwise the port name's prefix. A wrong or missing declaration does
not fail at render time. It shows up as calls the hop downgraded or refused.

| Port | Rendered when | Name | Declared protocol | Listener |
| --- | --- | --- | --- | --- |
| 9090 | always | `grpc-port` | `grpc` (name prefix) | the gRPC server |
| REST port | REST endpoint declared or scaffolded | `http-port` | `appProtocol: http` | plain `http.Server`: HTTP/1.1 only |
| Connect port | Connect endpoint declared or scaffolded | `connect-port` | `appProtocol: http2` | `h2c.NewHandler`: gRPC, Connect and gRPC-Web over cleartext HTTP/2, plus HTTP/1.1 |
| named endpoint | per declared named endpoint | the endpoint's name | from the endpoint API, below | the service's own |

**REST is `http`, never HTTP/2.** The generated REST listener refuses
prior-knowledge HTTP/2, which is what an upstream declared `http2` or
`kubernetes.io/h2c` sends, so every REST call through such a hop would fail. A
service that multiplexes gRPC onto its REST port has edited generated output.
The listener this agent generates does not do that, and relabelling the port
cannot make it.

**Connect is `http2`.** `connect-` is not a prefix a mesh recognises, so without
`appProtocol` an L7 hop has nothing to honour and can forward native gRPC to
it as HTTP/1.1. `http2` and the Kubernetes-standard `kubernetes.io/h2c`
(KEP-3726) are equivalent in Istio: both map to HTTP/2 in `ConvertProtocol`
(`pkg/config/kube/conversion.go`, checked at the 1.30.3 the fleet pins).
`http2` is used because more consumers recognise it, and this template ships to
arbitrary clusters.

**Named ports** are classified from their declared endpoint API. Connect
endpoints declare `appProtocol: http2`, and HTTP, REST and MCP endpoints
(Core's `standards.IsHTTPBasedAPI`) declare `appProtocol: http`. The agent
does not generate a named endpoint's listener, so the declaration follows the
API the service declared, exactly as the `http` derivation does. A service that
declares a Connect endpoint and serves plain HTTP/1.1 on it contradicts its own
declaration, and the manifest does not paper over that. gRPC and TCP ports are
unchanged.

Names and resolved ports are preserved, and a name alone never establishes the
protocol. Consumer manifests and overlays select ports by name, so a protocol
change goes in `appProtocol`, never in a rename.

Protocol metadata does not establish mesh enrollment, caller identity, header
sanitization or authorization policy. Those require their own infrastructure
configuration and consumer qualification.

`TestDeploymentPortsMatchDeclaredEndpoints` pins each conventional Service
port's name, number and `appProtocol` to its listener. It fails if REST stops
declaring `http` or declares HTTP/2, and if Connect stops declaring `http2`.
`TestServicePortsDeclareHTTPFromTheEndpointAPI` checks the named-port
derivation, including `http2` on a named Connect endpoint. Both render the real templates. They are renderer evidence, not a
live proxy negotiation test.

The current Core dependency selects grpc-gateway 2.33, whose default matcher no
longer forwards arbitrary `Grpc-Metadata-*` headers. Applications that carry
custom request metadata must declare the specific mapping through
`Configuration.ServeMuxOptions`. The generated gateway keeps that secure default;
image-role selection does not restore legacy header forwarding.
