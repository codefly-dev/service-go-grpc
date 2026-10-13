# Service protocol metadata

The deployment renderer sets `appProtocol: http` for the conventional REST
listener and named HTTP, REST and MCP listeners. Named ports are classified
from their declared endpoint API using Core's `standards.IsHTTPBasedAPI`, with
Connect excluded. Names and resolved ports are preserved; a name alone never
establishes the protocol.

Connect also serves native gRPC over cleartext HTTP/2 (h2c). Explicit `http`
metadata can force HTTP/1.1 upstream in a mesh, so Connect keeps its existing
protocol negotiation. gRPC and TCP ports are unchanged as well.

Protocol metadata does not establish mesh enrollment, caller identity, header
sanitization or authorization policy. Those require their own infrastructure
configuration and consumer qualification.

`TestServicePortsDeclareHTTPFromTheEndpointAPI` renders the real deployment
templates and checks protocol metadata and unchanged TCP/target ports. It also
refuses a change that adds `http` metadata to either conventional or named
Connect ports. This is renderer evidence, not a live proxy negotiation test.
