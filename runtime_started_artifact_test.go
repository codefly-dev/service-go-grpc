package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/contract"
	"github.com/codefly-dev/core/agents/services"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/stretchr/testify/require"
)

// selfReportingSource is a service that records the absolute path of the
// program it is running as, then keeps listening. It is the only way a test can
// hold the invariant that matters — that the path the agent REPORTS is the file
// that actually ran — without trusting the same accessor the agent trusts.
const selfReportingSource = `package main

import (
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	exe, err := os.Executable()
	if err != nil {
		os.Exit(2)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		os.Exit(3)
	}
	if err = os.WriteFile("ran-as.txt", []byte(resolved), 0o600); err != nil {
		os.Exit(4)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}
`

// ranAs waits for the started binary to record the program it is running as and
// returns that path. The process writes the file into its working directory,
// which is the service's source directory.
func ranAs(t *testing.T, sourceDir string) string {
	t.Helper()
	var recorded string
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(filepath.Join(sourceDir, "ran-as.txt"))
		if err != nil {
			return false
		}
		recorded = strings.TrimSpace(string(raw))
		return recorded != ""
	}, 10*time.Second, 20*time.Millisecond, "the started binary never recorded the program it ran as")
	return recorded
}

// reportedArtifact reads the artifact off a StartResponse the way a consumer
// does, through core's reader, which separates "the agent reported one" from
// "the agent reports none".
func reportedArtifact(t *testing.T, resp *runtimev0.StartResponse) (*runtimev0.StartedArtifact, bool) {
	t.Helper()
	return services.StartedArtifact(resp.GetStatus())
}

// TestNativeStartReportsTheBinaryItRan: a native start names the executable it
// launched, and names the file that is actually running — not a path derived
// from this agent's cache layout. A consumer (cli#993) digests the reported
// file and approves that build for the workload, so a report naming anything
// but the running program approves a build nothing is at.
func TestNativeStartReportsTheBinaryItRan(t *testing.T) {
	ctx := context.Background()
	runtime, sourceDir := newStartTestRuntime(t, selfReportingSource, false)

	resp, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState())

	artifact, reported := reportedArtifact(t, resp)
	require.True(t, reported, "a native start must report the artifact it ran")
	require.Empty(t, artifact.GetSources(),
		"a compiled runtime's executable IS the build, so it names no sources")

	exe := artifact.GetExecutable()
	require.True(t, filepath.IsAbs(exe), "the contract requires an absolute path, got %q", exe)
	info, err := os.Stat(exe)
	require.NoError(t, err, "the reported executable must be a file a consumer can digest")
	require.True(t, info.Mode().IsRegular(), "the reported executable must be a regular file")

	// The binary itself says what it is running as; that is the file the
	// consumer's digest has to be taken over. Both sides are resolved through
	// their symlinks because a path reached as /var and reported as
	// /private/var is one file on macOS.
	resolvedReport, err := filepath.EvalSymlinks(exe)
	require.NoError(t, err)
	require.Equal(t, ranAs(t, sourceDir), resolvedReport,
		"the agent reported a different file from the one it started")

	// Information answers for the process that is running, so it carries the
	// same artifact: the CLI reads it there on every poll, not just at Start.
	information, err := runtime.Information(ctx, &runtimev0.InformationRequest{})
	require.NoError(t, err)
	require.Equal(t, exe, information.GetStartStatus().GetStartedArtifact().GetExecutable())
}

// TestEveryNativeStartReportsItsOwnBinary: core CONSUMES the report when it
// stamps it on a StartResponse, because an artifact describes one process. An
// agent that reported only at its first start would leave every restart with
// nothing — refused by a consumer, which is why the report lives on the start
// path and not in Init.
func TestEveryNativeStartReportsItsOwnBinary(t *testing.T) {
	ctx := context.Background()
	runtime, sourceDir := newStartTestRuntime(t, longRunningSource, true)

	first, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, first.GetStatus().GetState())
	firstArtifact, reported := reportedArtifact(t, first)
	require.True(t, reported, "the first start must report its artifact")

	// A rebuild-replace: different source, so a genuinely different binary at
	// the same path. The restart must report again.
	writeServiceSource(t, sourceDir, longRunningSourceV2)
	second, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, second.GetStatus().GetState())
	secondArtifact, reported := reportedArtifact(t, second)
	require.True(t, reported, "a restart that reports nothing has the previous build approved for it")
	require.Equal(t, firstArtifact.GetExecutable(), secondArtifact.GetExecutable(),
		"the runner reuses one path per build environment, so only the file's content differs")
}

// TestFailedStartReportsNoArtifact: nothing started, so there is nothing to
// approve. A compile failure must not carry an artifact — and must not leave
// the previous process's artifact standing for a process that is gone.
func TestFailedStartReportsNoArtifact(t *testing.T) {
	ctx := context.Background()
	runtime, sourceDir := newStartTestRuntime(t, brokenSource, false)

	resp, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_ERROR, resp.GetStatus().GetState())
	_, reported := reportedArtifact(t, resp)
	require.False(t, reported, "a failed start must report no artifact")

	// The same runtime after a success: the success reports, and a later
	// failure takes its report away rather than inheriting the one before it.
	writeServiceSource(t, sourceDir, longRunningSource)
	started, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, started.GetStatus().GetState())
	_, reported = reportedArtifact(t, started)
	require.True(t, reported)

	writeServiceSource(t, sourceDir, brokenSource)
	failed, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_ERROR, failed.GetStatus().GetState())
	_, reported = reportedArtifact(t, failed)
	require.False(t, reported,
		"a failed rebuild must not leave the stopped binary reported as what is running")
}

// TestAdvertisementAdoptsRuntimeStartedArtifact: the capability is a claim about
// this agent's own behaviour, and core never advertises it on an agent's behalf.
// Start calls ReportStartedArtifact, so the advertisement says so — and an agent
// that stopped reporting must fail here rather than keep a promise a consumer
// trusts.
func TestAdvertisementAdoptsRuntimeStartedArtifact(t *testing.T) {
	info, err := NewService().GetAgentInformation(context.Background(), &agentv0.AgentInformationRequest{})
	require.NoError(t, err)
	require.Contains(t, info.GetContract().GetCapabilities(), contract.RuntimeStartedArtifact)
	require.True(t, slices.Contains(info.GetContract().GetCapabilities(), contract.DeploymentCompositionProvenance),
		"the capabilities this agent already advertised must survive")
}
