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
  `GoRunnerEnvironment.HostExecutable()` (core#810) answers with the executable
  the last successful `BuildBinary` produced as the **host** sees it, and
  refuses rather than offering a path to substitute. Deriving it here is exactly
  what the contract exists to end: the CLI used to derive every workload's build
  from the Go agents' `.cache/native/main`, so a service on any other agent was
  never delivered ([cli#989](https://github.com/codefly-dev/cli/issues/989)).
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
- **Nothing is reported when the runner refuses, and this agent substitutes
  nothing.** `HostExecutable` returns false for three reasons, each of which
  makes a report untruthful rather than merely missing: no build has succeeded;
  the build was a Docker companion's, whose executable exists only inside that
  container (narrower than "the container runtime context" — with no Docker
  engine the companion backend falls back to nix or local, the build lands in
  the host cache, and the agent does report it); or the recorded path is not
  absolute, which the consumer would refuse anyway since it resolves nothing
  against a user's terminal. The agent logs the refusal at `Warn` with the
  runtime context, because this is the one configuration where it advertises the
  capability and reports nothing — and the consumer's refusal then tells the
  user to implement a contract this agent does implement. Native delivery of a
  container-backed workload is core's and the CLI's question, not this agent's
  (the CLI's native cluster is a no-op under the container runtime context
  today).

The capability is advertised on `GetAgentInformation`, which is a claim about
this Runtime's own behaviour: core never advertises it for an agent built on it,
because an agent that claims it and reports nothing is the false promise a
consumer trusts.

## The tests that hold this

`runtime_started_artifact_test.go` builds and runs real binaries:

| Test | Invariant |
| --- | --- |
| `TestNativeStartReportsTheBinaryItRan` | the reported path is absolute, a regular file, and the file the started process itself says it is running as — not a path that merely looks right. It also asserts the advertised capability, so the claim and the behaviour cannot drift apart |
| `TestEveryNativeStartReportsItsOwnBinary` | a rebuild-replace restart reports again, and reports the file the process now running says it is running as, with different bytes from the first build. The runner reuses one path per build environment, so comparing paths alone would prove nothing about which build is named |
| `TestFailedStartReportsNoArtifact` | a compile failure reports nothing, and does not inherit the previous success's report |
| `TestTeardownWaitsForAStartAttempt` | `Stop` and `Destroy` wait for an attempt in flight, so a teardown cannot be overwritten by that attempt's STARTED and have a build approved for a process it killed |

`TestAdvertisementAdoptsItsOperationContracts` (in `provenance_contract_test.go`)
holds that the capability is advertised at all, beside this agent's other
operation-contract claims and the base contract.

The pin this needs is a core release, never a `replace`:
`TestTheCorePinIsAReleaseNotAReplace` fails while one stands, because a replace
is invisible to the dependency-lock gate and GoReleaser builds from the tag — a
release merged with one publishes an agent whose module graph points away from
core.

Core's own contract and reader live in `agents/services/base_runtime.go` and
`agents/services/runtime_status.go`; the consumer is the CLI's
`pkg/gitops/native_delivery.go` (`StartedArtifactOf`), which digests the
reported executable as the workload's approved build.
