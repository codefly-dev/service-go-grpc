package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/languages"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
	"github.com/stretchr/testify/require"
)

// The four shapes of user code a start has to tell apart: source that does not
// compile, a service that keeps listening, and a binary that is gone the moment
// it is launched — cleanly or not.
const (
	brokenSource = `package main

func main() {
`

	longRunningSource = `package main

import (
	"os"
	"os/signal"
	"syscall"
)

func main() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}
`

	// A second long-running variant so a rebuild produces a different binary
	// hash and therefore a genuinely new process.
	longRunningSourceV2 = `package main

import (
	"os"
	"os/signal"
	"syscall"
)

func main() {
	_ = "v2"
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}
`

	cleanExitSource = `package main

func main() {}
`

	failedExitSource = `package main

import "os"

func main() {
	os.Exit(3)
}
`
)

func writeServiceSource(t *testing.T, sourceDir, source string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "main.go"), []byte(source), 0o600))
}

// newStartTestRuntime binds a Runtime to a real native Go toolchain over a
// throwaway module, so Start runs a real `go build` and launches a real
// process. Returns the runtime and its source directory.
func newStartTestRuntime(t *testing.T, source string, hotReload bool) (*Runtime, string) {
	t.Helper()
	if !languages.HasGoRuntime(nil) {
		t.Skip("native go toolchain not available")
	}
	ctx := context.Background()

	workspace := t.TempDir()
	sourceDir := filepath.Join(workspace, "code")
	require.NoError(t, os.MkdirAll(sourceDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "go.mod"), []byte("module testsvc\n\ngo 1.27\n"), 0o600))
	writeServiceSource(t, sourceDir, source)

	env, err := golanghelpers.NewNativeGoRunner(ctx, workspace, "code")
	require.NoError(t, err)
	env.WithLocalCacheDir(filepath.Join(workspace, ".cache"))
	require.NoError(t, env.Init(ctx))

	runtime := NewRuntime(NewService())
	runtime.Base.Logger = runtime.Base.Wool
	runtime.RunnerEnvironment = env
	runtime.GoGrpc.Settings.HotReload = hotReload
	t.Cleanup(func() {
		_, _ = runtime.Stop(ctx, &runtimev0.StopRequest{})
	})
	return runtime, sourceDir
}

// observedStatus reads StartStatus the way the Information RPC does — through
// the RuntimeWrapper's lock, so it is safe to sample while a supervise
// goroutine may be writing.
func observedStatus(s *Runtime) (runtimev0.StartStatus_Status, string) {
	s.Base.Runtime.RLock()
	defer s.Base.Runtime.RUnlock()
	if s.Base.Runtime.StartStatus == nil {
		return runtimev0.StartStatus_UNKNOWN, ""
	}
	return s.Base.Runtime.StartStatus.State, s.Base.Runtime.StartStatus.Message
}

func requireEventualStatus(t *testing.T, s *Runtime, want runtimev0.StartStatus_Status) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var state runtimev0.StartStatus_Status
	var message string
	for time.Now().Before(deadline) {
		state, message = observedStatus(s)
		if state == want {
			return message
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("status never reached %v (last %v: %s)", want, state, message)
	return ""
}

// requireStableStatus asserts the status still says want after every goroutine
// a start attempt spawned has had time to run — this is where a late exit
// report would show up.
func requireStableStatus(t *testing.T, s *Runtime, want runtimev0.StartStatus_Status) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, message := observedStatus(s)
		require.Equalf(t, want, state, "status drifted to %v: %s", state, message)
		time.Sleep(20 * time.Millisecond)
	}
}

// TestStartCompileFailureIsNeverStarted pins the core of the fix: source that
// does not compile leaves the service non-ready and carries the compiler's own
// diagnostics. Hot reload used to swallow this and answer STARTED, so the CLI
// treated a service with no binary at all as successfully launched.
func TestStartCompileFailureIsNeverStarted(t *testing.T) {
	for _, hotReload := range []bool{true, false} {
		t.Run(fmt.Sprintf("hot-reload=%v", hotReload), func(t *testing.T) {
			ctx := context.Background()
			runtime, _ := newStartTestRuntime(t, brokenSource, hotReload)

			resp, err := runtime.Start(ctx, &runtimev0.StartRequest{})
			require.NoError(t, err)
			require.Equal(t, runtimev0.StartStatus_ERROR, resp.GetStatus().GetState())
			require.Contains(t, resp.GetStatus().GetMessage(), "compilation failed")
			require.Contains(t, resp.GetStatus().GetMessage(), "syntax error",
				"the compiler's diagnostics must survive into the status message")
			require.Nil(t, runtime.runner, "a failed compile must not leave a runner behind")

			info, err := runtime.Information(ctx, &runtimev0.InformationRequest{})
			require.NoError(t, err)
			require.Equal(t, runtimev0.StartStatus_ERROR, info.GetStartStatus().GetState(),
				"the polled status must agree with the Start response")
			require.Contains(t, info.GetStartStatus().GetMessage(), "syntax error")
		})
	}
}

// TestHotReloadRebuildsAfterSourceFix covers the other half: reporting the
// failure truthfully must not cost the retry. The same runtime rebuilds and
// serves once the source compiles, and breaking it again takes readiness away.
func TestHotReloadRebuildsAfterSourceFix(t *testing.T) {
	ctx := context.Background()
	runtime, sourceDir := newStartTestRuntime(t, brokenSource, true)

	resp, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_ERROR, resp.GetStatus().GetState())

	writeServiceSource(t, sourceDir, longRunningSource)
	resp, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState())
	requireStableStatus(t, runtime, runtimev0.StartStatus_STARTED)

	// An error introduced after a successful run stops the binary that was
	// serving and produces no replacement: readiness must not survive that.
	writeServiceSource(t, sourceDir, brokenSource)
	resp, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_ERROR, resp.GetStatus().GetState())
	requireStableStatus(t, runtime, runtimev0.StartStatus_ERROR)
}

// TestStartMarksImmediateBinaryExitFailed exercises the ordering fix with real
// processes. Launching succeeds, so Start answers STARTED, but the binary is
// already gone; supervision is armed after that commit, so its ERROR lands last
// and the terminal status is a failure — including for a status-0 exit, which
// for a service that is supposed to keep listening is just as fatal.
func TestStartMarksImmediateBinaryExitFailed(t *testing.T) {
	cases := map[string]string{
		"clean-exit":   cleanExitSource,
		"failure-exit": failedExitSource,
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			runtime, _ := newStartTestRuntime(t, source, false)

			resp, err := runtime.Start(ctx, &runtimev0.StartRequest{})
			require.NoError(t, err)
			require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState())

			requireEventualStatus(t, runtime, runtimev0.StartStatus_ERROR)
			requireStableStatus(t, runtime, runtimev0.StartStatus_ERROR)
		})
	}
}

// TestRapidRebuildKeepsReplacementReady runs the rebuild-replace path with real
// processes: the superseded binary is killed and its exit is reported while the
// replacement is already serving. The replacement must stay ready.
func TestRapidRebuildKeepsReplacementReady(t *testing.T) {
	ctx := context.Background()
	runtime, sourceDir := newStartTestRuntime(t, longRunningSource, true)

	resp, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState())

	writeServiceSource(t, sourceDir, longRunningSourceV2)
	resp, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState())

	requireStableStatus(t, runtime, runtimev0.StartStatus_STARTED)
}

// TestSupersededRunnerExitCannotFailNewerGeneration pins the generation guard
// directly, for the interleaving a real rebuild cannot be made to produce on
// demand: the old runner's supervise goroutine gets to report only after the
// replacement has already committed STARTED.
func TestSupersededRunnerExitCannotFailNewerGeneration(t *testing.T) {
	runtime := NewRuntime(NewService())

	superseded := runtime.nextGeneration()
	_, err := runtime.commitStarted()
	require.NoError(t, err)

	runtime.nextGeneration()
	_, err = runtime.commitStarted()
	require.NoError(t, err)

	runtime.reportRunnerExit(superseded, errors.New("old binary exited late"))

	state, message := observedStatus(runtime)
	require.Equalf(t, runtimev0.StartStatus_STARTED, state,
		"a superseded runner's exit must not fail the replacement: %s", message)
}

func TestCurrentRunnerExitFailsTheService(t *testing.T) {
	runtime := NewRuntime(NewService())

	gen := runtime.nextGeneration()
	_, err := runtime.commitStarted()
	require.NoError(t, err)

	// A clean exit is still an exit: nobody asked the binary to stop.
	runtime.reportRunnerExit(gen, nil)

	state, message := observedStatus(runtime)
	require.Equal(t, runtimev0.StartStatus_ERROR, state)
	require.Equal(t, "runner exited", message)
}

// TestReplacementRevokesReadiness covers the window a rebuild opens: the binary
// that was serving is stopped and its replacement does not exist yet, so a
// status poll landing in there must not still read STARTED.
func TestReplacementRevokesReadiness(t *testing.T) {
	runtime := NewRuntime(NewService())

	runtime.nextGeneration()
	_, err := runtime.commitStarted()
	require.NoError(t, err)

	runtime.revokeReadiness("rebuilding")

	state, message := observedStatus(runtime)
	require.NotEqual(t, runtimev0.StartStatus_STARTED, state)
	require.Equal(t, "rebuilding", message)
}
