# Native ownership qualification

The go-grpc runtime uses Core's native Go runner for process creation, authenticated
ownership registration and normal Stop. Core owns process birth inspection and
identity-bound signaling; the CLI owns explicit orphan-recovery selection. Updating
this agent's Core dependency adopts that writer in the built agent. Updating a
source checkout alone does not change an already installed or running agent.

`TestNativeAgentOwnershipLifecycle` consumes the output of the maintained
`codefly agent build --native-only` command. It adapts service-go's native lifecycle
proof to this agent's Load-reported endpoints and Core's network allocator. Run it
with an independently selected CLI and an isolated agent installation:

```sh
SERVICE_GO_GRPC_NATIVE_AGENT_HOME=/absolute/isolated/codefly-home \
SERVICE_GO_GRPC_QUALIFICATION_CLI=/absolute/reviewed/codefly \
go test . -run '^TestNativeAgentOwnershipLifecycle$' -count=1 -v
```

Build with an isolated OS `HOME` and `CODEFLY_HOME` too. The test creates its own
temporary OS home for the process registry and uses the supplied installation for
agent loading. It records binary, adjacent SBOM and CLI SHA256 digests. Its two
disposable workspaces launch independently authenticated agent owners through
Core's manager and invoke ordinary gRPC Load, Init, Start and Stop. Normal Stop
must remove the workload and registration. The first runtime then restarts; Core's
identity-bound API kills only that disposable agent owner. The existing CLI
previews the surviving orphan and recovers it using the unchanged preview receipt
as `--expect`. The sibling's exact leader birth and live owner must survive both
operations, then stop normally.

This gate bypasses sandbox and principal policy, as in service-go's proof. It
qualifies agent transport authentication and native workload lifetime, not those
policies, full CLI composition, a published release, or a different OS. The small
workload is a lifetime fixture; generated gRPC/REST/Connect listener behavior is
covered separately by the base service and factory tests. Docker, cross packaging,
Nix and protected incident environments require their own explicit qualification.

Dependency adoption uses `codefly agent deps --pin <published-core-version>` so
the root module, `base/code` and factory locks move together; update the reusable
CI workflow ref in the same change. A reviewed local Core workspace is separate
source evidence and requires a source builder that honors the CLI's prepared
`GOWORK`. Never treat its source path as a published dependency or infer adoption
from a successful build: retain source revision, module selection, binary build
metadata and the SBOM's executable digest with the actual lifecycle result.

The source packager is selected independently in `agent.codefly.yaml`; the
service-go module embedded by the runtime remains its existing version. For local
workspace qualification, build the reviewed service-go source packager through
its own `agent build --native-only` command into the same isolated installation.
A manifest version is only an installation key: record the packager's source
revision and digest, because a local build need not equal that published release.
