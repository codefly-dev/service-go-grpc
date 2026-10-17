package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/contract"
	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
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

// selfReportingSourceV2 is the same fixture with different content, so a
// rebuild produces a genuinely different executable at the runner's one path.
const selfReportingSourceV2 = `package main

import (
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	_ = "v2"
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
// which is the service's source directory. A caller testing a restart deletes
// the record first (forgetRanAs), so the answer cannot be the previous
// process's.
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

// forgetRanAs drops the record of what ran, so the next ranAs can only be
// answered by a process started after this point.
func forgetRanAs(t *testing.T, sourceDir string) {
	t.Helper()
	require.NoError(t, os.Remove(filepath.Join(sourceDir, "ran-as.txt")))
}

// digestOf is the value a consumer approves for a compiled runtime: the
// sha256 of the executable the agent named.
func digestOf(t *testing.T, path string) string {
	t.Helper()
	payload, err := os.ReadFile(path)
	require.NoError(t, err)
	return fmt.Sprintf("%x", sha256.Sum256(payload))
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
	require.Equal(t, ranAs(t, sourceDir), mustEvalSymlinks(t, exe),
		"the agent reported a different file from the one it started")

	// Information answers for the process that is running, so it carries the
	// same artifact: the CLI reads it there on every poll, not just at Start.
	information, err := runtime.Information(ctx, &runtimev0.InformationRequest{})
	require.NoError(t, err)
	require.Equal(t, exe, information.GetStartStatus().GetStartedArtifact().GetExecutable())

	// The advertisement is asserted HERE, beside the behaviour that earns it:
	// a capabilities check of its own passes just as well when nothing reports,
	// and an agent that advertises the contract while reporting nothing is the
	// false promise a consumer trusts.
	advertised, err := NewService().GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})
	require.NoError(t, err)
	require.Contains(t, advertised.GetContract().GetCapabilities(), contract.RuntimeStartedArtifact,
		"this Start reports an artifact, so the advertisement must say so")
}

// TestEveryNativeStartReportsItsOwnBinary: core CONSUMES the report when it
// stamps it on a StartResponse, because an artifact describes one process. An
// agent that reported only at its first start would leave every restart with
// nothing — refused by a consumer, which is why the report lives on the start
// path and not in Init.
//
// The restart's report is checked against the process now running, and the
// executable's bytes are checked to have changed. The runner keeps one path per
// build environment, so a report that merely repeated the first start's string
// would name the right path by construction; what makes it the CURRENT build is
// that the process answering from that path is the new one.
func TestEveryNativeStartReportsItsOwnBinary(t *testing.T) {
	ctx := context.Background()
	runtime, sourceDir := newStartTestRuntime(t, selfReportingSource, true)

	first, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, first.GetStatus().GetState())
	firstArtifact, reported := reportedArtifact(t, first)
	require.True(t, reported, "the first start must report its artifact")
	require.Equal(t, ranAs(t, sourceDir), mustEvalSymlinks(t, firstArtifact.GetExecutable()))
	firstDigest := digestOf(t, firstArtifact.GetExecutable())

	// A rebuild-replace: different source, so a genuinely different binary at
	// the same path. Forget what the first process recorded, so the assertion
	// below can only be answered by the process this start launches.
	forgetRanAs(t, sourceDir)
	writeServiceSource(t, sourceDir, selfReportingSourceV2)
	second, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, second.GetStatus().GetState())
	secondArtifact, reported := reportedArtifact(t, second)
	require.True(t, reported, "a restart that reports nothing has the previous build approved for it")

	require.Equal(t, ranAs(t, sourceDir), mustEvalSymlinks(t, secondArtifact.GetExecutable()),
		"the restart must report the file the process now running is running as")
	require.NotEqual(t, firstDigest, digestOf(t, secondArtifact.GetExecutable()),
		"the rebuild must have produced different bytes, or this test proves nothing about the restart")
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

// mustEvalSymlinks resolves a path the way the first test's fixture does, so a
// workspace reached as /var and reported as /private/var compares equal.
func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return resolved
}

// TestTeardownWaitsForAStartAttempt: a Stop or Destroy that interleaved with a
// start attempt would be overwritten by that attempt's STARTED — and, since the
// attempt also reports what it launched, the consumer would digest the named
// file and register an approved build for a process the teardown had just
// killed. Destroy is worse: it removes the cache directory the reported
// executable lives in. Both therefore wait for the attempt that owns runner and
// runnerCancel to finish.
//
// Driven through the lock rather than through a real race, because the
// interleaving cannot be produced on demand: holding startMu is exactly the
// state "an attempt is in flight".
func TestTeardownWaitsForAStartAttempt(t *testing.T) {
	for name, teardown := range map[string]func(*Runtime) error{
		"stop": func(s *Runtime) error {
			_, err := s.Stop(context.Background(), &runtimev0.StopRequest{})
			return err
		},
		"destroy": func(s *Runtime) error {
			_, err := s.Destroy(context.Background(), &runtimev0.DestroyRequest{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			runtime := NewRuntime(NewService())
			runtime.Base.Logger = runtime.Base.Wool
			// Enough of a loaded service for each teardown to run its real
			// body over throwaway state: Destroy empties cacheLocation and
			// reads the identity, and without these it would fault into
			// Wool.Catch and this test would assert against a recovered panic
			// instead of against the teardown.
			require.NoError(t, runtime.SetRuntimeContext(context.Background(), resources.NewRuntimeContextNative()))
			runtime.Base.Runtime.SetEnvironment(&basev0.Environment{Name: "local"})
			runtime.Base.Identity = &resources.ServiceIdentity{
				Name: "svc", Module: "mod", Workspace: "ws",
				WorkspacePath: t.TempDir(), RelativeToWorkspace: "svc",
			}
			runtime.cacheLocation = t.TempDir()

			runtime.startMu.Lock()
			returned := make(chan error, 1)
			go func() { returned <- teardown(runtime) }()

			select {
			case err := <-returned:
				runtime.startMu.Unlock()
				t.Fatalf("teardown ran while a start attempt held the lock (err=%v)", err)
			case <-time.After(250 * time.Millisecond):
			}

			runtime.startMu.Unlock()
			select {
			case <-returned:
			case <-time.After(10 * time.Second):
				t.Fatal("teardown never completed after the start attempt released the lock")
			}
		})
	}
}
