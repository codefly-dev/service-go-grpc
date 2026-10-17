# The artifact a native start reports

A native `codefly run` approves a build for every workload it delivers presence
for, the way a cell approves an OCI image manifest. For a native process the
build is the file that is running, and only the agent that started it knows
which file that is. So the agent says it: `Runtime.Start` calls
`RuntimeWrapper.ReportStartedArtifact` before its `StartResponse`, and core
stamps the report on `StartStatus.started_artifact`
(`runtime-started-artifact/v1`, core#796).

This agent is a compiled runtime: it builds one executable and runs it, so the
executable **is** the build and `sources` stays empty. An interpreted runtime —
python-fastapi — reports its interpreter plus the sources it was handed, because
an interpreter identifies nothing about the service.

## What the rules are, and why each one is load-bearing

- **The path comes from the runner, never from this agent's layout.**
  `GoRunnerEnvironment.Executable()` answers with the host path of the last
  successful `BuildBinary`. Deriving it here is exactly what the contract
  exists to end: the CLI used to derive every workload's build from the Go
  agents' `.cache/native/main`, so a service on any other agent was never
  delivered ([cli#989](https://github.com/codefly-dev/cli/issues/989)).
- **Report before `StartResponse`.** `StartResponse` stamps the report on the
  status *and consumes it* — a report describes one process. A report made
  after the commit belongs to no start at all.
- **Every start reports, restarts included.** Because the report is consumed, an
  agent that reported only at its first start leaves a hot-reload restart with
  nothing. The consumer then refuses the workload by name rather than approving
  the stopped binary's build for the process now running.
- **A failed start reports nothing.** Core's `StartError` discards any pending
  report, and this agent reports only after `runner.Start` has returned without
  error, so a compile failure or a launch failure names no build.
- **Only an absolute path is reported.** The consumer resolves no relative path
  — it would resolve against the user's terminal — and refuses one by naming
  this agent. A relative path from the runner is therefore logged and dropped
  rather than reported.
- **A container-backed start reports nothing.** Under the container runtime
  context the build runs inside a Docker companion, whose `/build` path names no
  file on the host; `Executable()` answers empty and this agent reports nothing
  rather than a path the consumer cannot open. Native delivery of a
  container-backed workload is core's question, not this agent's.

The capability is advertised on `GetAgentInformation`, which is a claim about
this Runtime's own behaviour: core never advertises it for an agent built on it,
because an agent that claims it and reports nothing is the false promise a
consumer trusts.

## The tests that hold this

`runtime_started_artifact_test.go` builds and runs real binaries:

| Test | Invariant |
| --- | --- |
| `TestNativeStartReportsTheBinaryItRan` | the reported path is absolute, a regular file, and the file the started process itself says it is running as — not a path that merely looks right |
| `TestEveryNativeStartReportsItsOwnBinary` | a rebuild-replace restart reports again |
| `TestFailedStartReportsNoArtifact` | a compile failure reports nothing, and does not inherit the previous success's report |
| `TestAdvertisementAdoptsRuntimeStartedArtifact` | the advertised capability keeps matching what `Start` does |

Core's own contract and reader live in `agents/services/base_runtime.go` and
`agents/services/runtime_status.go`; the consumer is the CLI's
`pkg/gitops/native_delivery.go` (`StartedArtifactOf`), which digests the
reported executable as the workload's approved build.
