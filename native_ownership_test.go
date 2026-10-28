package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/manager"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/standards"
)

// An agent can serve successfully while silently omitting its ownership record.
// Adapted from service-go159: this opt-in gate consumes the actual `agent build --native-only` output.
// It never builds a substitute agent, touches the user's registry, or selects
// processes by name. Both services and their agent owners are disposable.
func TestNativeAgentOwnershipLifecycle(t *testing.T) {
	agentHome := os.Getenv("SERVICE_GO_GRPC_NATIVE_AGENT_HOME")
	cli := os.Getenv("SERVICE_GO_GRPC_QUALIFICATION_CLI")
	if agentHome == "" && cli == "" {
		t.Skip("set SERVICE_GO_GRPC_NATIVE_AGENT_HOME and SERVICE_GO_GRPC_QUALIFICATION_CLI to qualify built artifacts")
	}
	if !filepath.IsAbs(agentHome) || !filepath.IsAbs(cli) {
		t.Fatal("qualification inputs must be absolute paths")
	}
	t.Setenv("HOME", t.TempDir()) // Core's process registry is OS-home scoped.
	t.Setenv(resources.CodeflyHomeEnv, agentHome)
	t.Setenv("GOWORK", "off")
	installed, err := agent.Path(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{installed, installed + ".cdx.json", cli} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("artifact %s sha256=%x", path, sha256.Sum256(body))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	a := startOwnedNativeService(t, ctx, "first")
	b := startOwnedNativeService(t, ctx, "sibling")
	if a.conn.ProcessInfo().PID == b.conn.ProcessInfo().PID {
		t.Fatal("sibling must have an independently launched agent owner")
	}
	siblingOwner, err := runners.InspectProcess(b.conn.ProcessInfo().PID)
	if err != nil {
		t.Fatal(err)
	}
	assertNativeOwnership(t, ctx, a.group, a.workspace, true)
	assertNativeOwnership(t, ctx, b.group, b.workspace, true)

	// Ordinary Runtime.Stop must retire A without affecting B's exact birth.
	stopped, err := a.client.Stop(ctx, &runtimev0.StopRequest{})
	if err != nil || stopped.GetStatus().GetState() != runtimev0.StopStatus_SUCCESS {
		t.Fatalf("ordinary stop: %v, %v", stopped, err)
	}
	waitNativeCondition(t, func() bool { return !a.group.Alive() })
	if _, err := runners.LookupProcessGroup(a.group.PGID()); !errors.Is(err, runners.ErrProcessGroupNotRegistered) {
		t.Fatalf("ordinary stop retained the workload registration: %v", err)
	}
	assertNativeOwnership(t, ctx, b.group, b.workspace, true)
	t.Log("ordinary Stop removed first workload; sibling remains authenticated and supervised")

	// Restart through the same Runtime, then kill only its disposable owner
	// birth so no agent Stop/finalizer can mask recovery of an orphan workload.
	a.group = startNativeWorkload(t, ctx, a.client, a.workspace)
	owner, err := runners.InspectProcess(a.conn.ProcessInfo().PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := runners.SignalProcess(ctx, owner, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitNativeCondition(t, func() bool {
		ownership, err := a.group.InspectOwnership(ctx)
		return err == nil && ownership.Authenticated && !ownership.OwnerAlive
	})
	assertNativeOwnership(t, ctx, a.group, a.workspace, false)
	assertNativeOwnership(t, ctx, b.group, b.workspace, true)

	// Use the maintained CLI consumer and Core's strict receipt parser; do not
	// invent a recovery selection from registry JSON or a list of descendants.
	pgid := strconv.Itoa(a.group.PGID())
	preview, err := exec.CommandContext(ctx, cli, "recover", "process-group", pgid, "--preview").Output()
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	expected, err := runners.ParseProcessGroupRecoveryExpectation(preview)
	if err != nil || expected.Namespace != "authenticated-v2" || expected.PGID != a.group.PGID() || expected.Owner.PID != owner.PID {
		t.Fatalf("actual agent did not write the expected v2 registration: %+v, %v", expected, err)
	}
	t.Logf("orphan preview: %s", preview)
	assertNativeOwnership(t, ctx, a.group, a.workspace, false)
	receipt := filepath.Join(t.TempDir(), "recovery.json")
	if err := os.WriteFile(receipt, preview, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := exec.CommandContext(ctx, cli, "recover", "process-group", pgid, "--expect", receipt).CombinedOutput()
	if err != nil {
		t.Fatalf("recover: %v\n%s", err, result)
	}
	if a.group.Alive() {
		t.Fatal("recovery left the selected workload alive")
	}
	if _, err := runners.LookupProcessGroup(a.group.PGID()); !errors.Is(err, runners.ErrProcessGroupNotRegistered) {
		t.Fatalf("recovery did not remove the exact registration: %v", err)
	}
	assertNativeOwnership(t, ctx, b.group, b.workspace, true)
	currentSiblingOwner, err := runners.InspectProcess(b.conn.ProcessInfo().PID)
	if err != nil || currentSiblingOwner != siblingOwner {
		t.Fatalf("recovery changed the sibling owner birth: %+v, %v", currentSiblingOwner, err)
	}
	t.Logf("recovery result: %s; sibling preserved", result)
	stopped, err = b.client.Stop(ctx, &runtimev0.StopRequest{})
	if err != nil || stopped.GetStatus().GetState() != runtimev0.StopStatus_SUCCESS {
		t.Fatalf("sibling ordinary stop: %v, %v", stopped, err)
	}
	waitNativeCondition(t, func() bool { return !b.group.Alive() })
}

type ownedNativeService struct {
	workspace string
	conn      *manager.AgentConn
	client    runtimev0.RuntimeClient
	group     *runners.TrackedProcessGroup
}

func startOwnedNativeService(t *testing.T, ctx context.Context, name string) *ownedNativeService {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := &resources.Service{
		Name: name, Version: "0.0.1",
		Endpoints: []*resources.Endpoint{{Name: standards.GRPC, API: standards.GRPC}},
		Spec:      map[string]any{"hot-reload": false, "rest-endpoint": false},
	}
	if err := service.SaveAtDir(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(workspace, "code", "go.mod"), "module example.test/"+name+"\n\ngo 1.27.0\n")
	writeTestFile(t, filepath.Join(workspace, "code", "main.go"), `package main
import ("os"; "strconv"; "time")
func main() {
 if err := os.WriteFile("workload.pid", []byte(strconv.Itoa(os.Getpid())), 0600); err != nil { panic(err) }
 for { time.Sleep(time.Second) }
}
`)
	t.Setenv(runners.LaunchWorkspaceEnv, workspace)
	conn, err := manager.Load(ctx, agent, manager.WithoutSandbox(), manager.WithoutPrincipal(), manager.WithWorkDir(workspace))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	client := runtimev0.NewRuntimeClient(conn.GRPCConn())
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = client.Stop(cleanup, &runtimev0.StopRequest{})
	})
	environment, err := resources.LocalEnvironment().Proto()
	if err != nil {
		t.Fatal(err)
	}
	identity := &basev0.ServiceIdentity{Name: name, Module: "mymodule", Version: "0.0.1", WorkspacePath: workspace, RelativeToWorkspace: "."}
	loaded, err := client.Load(ctx, &runtimev0.LoadRequest{
		Identity:    identity,
		Environment: environment, DisableCatch: true,
	})
	if err != nil || loaded.GetStatus().GetState() != runtimev0.LoadStatus_READY {
		t.Fatalf("load: %v, %v\n%s", loaded, err, conn.StderrTail())
	}
	// Use the same endpoint allocator as the CLI; no fixture port or address
	// substitutes for the mapping reported by this agent's Load response.
	networks, err := network.NewRuntimeManager(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	networks.WithTemporaryPorts()
	mappings, err := networks.GenerateNetworkMappings(ctx, resources.LocalEnvironment(), &resources.Workspace{Name: "native-qualification"},
		resources.ServiceIdentityFromProto(identity),
		loaded.GetEndpoints(), resources.NewRuntimeContextNative())
	if err != nil {
		t.Fatal(err)
	}
	initialized, err := client.Init(ctx, &runtimev0.InitRequest{RuntimeContext: resources.NewRuntimeContextNative(), ProposedNetworkMappings: mappings})
	if err != nil || initialized.GetStatus().GetState() != runtimev0.InitStatus_READY {
		t.Fatalf("init: %v, %v\n%s", initialized, err, conn.StderrTail())
	}
	return &ownedNativeService{workspace, conn, client, startNativeWorkload(t, ctx, client, workspace)}
}

func startNativeWorkload(t *testing.T, ctx context.Context, client runtimev0.RuntimeClient, workspace string) *runners.TrackedProcessGroup {
	t.Helper()
	pidFile := filepath.Join(workspace, "code", "workload.pid")
	if err := os.Remove(pidFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	started, err := client.Start(ctx, &runtimev0.StartRequest{})
	if err != nil || started.GetStatus().GetState() != runtimev0.StartStatus_STARTED {
		t.Fatalf("start: %v, %v", started, err)
	}
	executable := started.GetStatus().GetStartedArtifact().GetExecutable()
	if !filepath.IsAbs(executable) {
		t.Fatalf("start did not report an absolute executable: %q", executable)
	}
	var pid int
	waitNativeCondition(t, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid > 1
	})
	group, err := runners.LookupProcessGroup(pid)
	if err != nil {
		t.Fatal(err)
	}
	assertNativeOwnership(t, ctx, group, workspace, true)
	if group.Leader().Executable != filepath.Base(executable) {
		t.Fatalf("registered executable does not match Start's artifact: %+v, %s", group.Leader(), executable)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = group.Terminate(cleanup)
		_ = group.RemoveIfDeadContext(cleanup)
	})
	return group
}

func assertNativeOwnership(t *testing.T, ctx context.Context, group *runners.TrackedProcessGroup, workspace string, ownerAlive bool) {
	t.Helper()
	ownership, err := group.InspectOwnership(ctx)
	if err != nil || !ownership.Authenticated || ownership.OwnerAlive != ownerAlive || ownership.LaunchWorkspace != workspace || len(ownership.Members) != 1 || ownership.Members[0] != group.Leader() {
		t.Fatalf("ownership for %d: %+v, %v", group.PGID(), ownership, err)
	}
}

func waitNativeCondition(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("native lifecycle postcondition did not arrive")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
