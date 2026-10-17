# Service ports in the deployment manifests

`templates/deployment/kustomize/base/service.yaml.tmpl` renders one Service
port per listener the process binds, and each port declares the protocol its
listener actually speaks. An L7 hop in front of the pod (an Istio waypoint, an
ingress) chooses the upstream protocol from that declaration: `appProtocol`
when set, otherwise the port name's prefix. A wrong or missing declaration does
not fail anywhere at render time; it shows up as calls that the hop downgraded
or refused.

| Port | Rendered when | Name | Declared protocol | Listener |
| --- | --- | --- | --- | --- |
| 9090 | always | `grpc-port` | `grpc` (name prefix) | the gRPC server |
| REST port | REST endpoint declared or scaffolded | `http-port` | `http` (name prefix) | plain `http.Server`: HTTP/1.1 only |
| Connect port | Connect endpoint declared or scaffolded | `connect-port` | `appProtocol: kubernetes.io/h2c` | `h2c.NewHandler`: gRPC, Connect and gRPC-Web over cleartext HTTP/2, plus HTTP/1.1 |
| named endpoint | per declared named endpoint | the endpoint's name | name prefix | the service's own |

The Connect listener needs `appProtocol` because `connect-` is not a prefix a
mesh recognises. Without it, an L7 hop can forward native gRPC to it as
HTTP/1.1. `kubernetes.io/h2c` is the value Kubernetes standardised for
cleartext HTTP/2 (KEP-3726). Istio maps it to HTTP/2 exactly as it maps
`http2` (`ConvertProtocol` in `pkg/config/kube/conversion.go`, checked at the
1.30.3 the fleet pins). It is also the value Gateway API backend protocol
selection (GEP-1911) defines.

The REST port must **not** declare HTTP/2. The generated REST listener refuses
prior-knowledge HTTP/2, which is what an upstream declared `http2` or
`kubernetes.io/h2c` sends, so every REST call through such a hop would fail. A
service that multiplexes gRPC onto its REST port has edited generated output;
the listener this agent generates does not do that.

Port names are part of the contract: consumer manifests and overlays select
ports by name, so a protocol change goes in `appProtocol`, never in a rename.

`TestDeploymentPortsMatchDeclaredEndpoints` renders the real templates. It pins
each Service port's name, number and declared protocol to its listener. It
fails if the Connect port's `appProtocol` is dropped or if the REST port is
declared h2c. This is render-time evidence. No test here drives traffic through
a mesh.
