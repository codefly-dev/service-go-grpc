package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/tools/imports"
)

// TestFormatStagedGoMatchesLint proves that generation and lint agree: after
// formatStagedGo rewrites a staged .go file, the exact goimports pass the lint
// runs reports it clean, and a second pass is a no-op.
func TestFormatStagedGoMatchesLint(t *testing.T) {
	stage := t.TempDir()
	unformatted := "package sample\n\nimport \"strings\"\nimport \"fmt\"\n\nfunc Use()  string {return fmt.Sprint(strings.ToUpper(\"x\"))}\n"
	goPath := filepath.Join(stage, "gen", "sample.go")
	writeTestFile(t, goPath, unformatted)
	writeTestFile(t, filepath.Join(stage, "proto", "api.proto"), "syntax = \"proto3\";")

	if err := formatStagedGo(stage); err != nil {
		t.Fatal(err)
	}

	formatted, err := os.ReadFile(goPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(formatted) == unformatted {
		t.Fatal("formatStagedGo left a non-goimports-clean file unchanged")
	}
	// The lint's own check must now find nothing to fix.
	fixed, err := imports.Process(goPath, formatted, &imports.Options{Comments: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(fixed) != string(formatted) {
		t.Fatalf("lint still reports drift after formatting:\n%s", fixed)
	}
	// Idempotent: a second sync produces no further change, so sync-drift passes.
	if err := formatStagedGo(stage); err != nil {
		t.Fatal(err)
	}
	assertTestFile(t, goPath, string(formatted))
	// Non-Go staged input is left untouched.
	assertTestFile(t, filepath.Join(stage, "proto", "api.proto"), "syntax = \"proto3\";")
}

// TestFormatStagedGoSkipsUnparseableFile verifies that a .go file goimports
// cannot parse does not abort the sync and is left untouched.
func TestFormatStagedGoSkipsUnparseableFile(t *testing.T) {
	stage := t.TempDir()
	broken := "package sample\n\nfunc oops( {\n"
	brokenPath := filepath.Join(stage, "gen", "broken.go")
	writeTestFile(t, brokenPath, broken)

	if err := formatStagedGo(stage); err != nil {
		t.Fatalf("unparseable staged file aborted formatting: %v", err)
	}
	assertTestFile(t, brokenPath, broken)
}

// TestFormatStagedGoLeavesSymlinkTargetUntouched verifies that a .go symlink is
// not dereferenced, so formatting cannot truncate a file outside the tree.
func TestFormatStagedGoLeavesSymlinkTargetUntouched(t *testing.T) {
	stage := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.go")
	unformatted := "package sample\n\nimport \"fmt\"\n\nfunc Use()  {fmt.Println()}\n"
	writeTestFile(t, outside, unformatted)

	link := filepath.Join(stage, "gen", "link.go")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	if err := formatStagedGo(stage); err != nil {
		t.Fatal(err)
	}
	assertTestFile(t, outside, unformatted)
}

func TestSyncTransactionDryRunPredictsAndAppliesExactTree(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "gen", "changed.go"), "before")
	writeTestFile(t, filepath.Join(root, "gen", "stale.go"), "stale")
	writeTestFile(t, filepath.Join(root, "proto", "buf.lock"), "old-lock")
	writeTestFile(t, filepath.Join(root, "proto", "obsolete.lock"), "obsolete-lock")
	writeTestFile(t, filepath.Join(root, "user.go"), "user-owned")

	transaction, err := newSyncTransaction(root, "modules/api")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transaction.Close() }()
	writeTestFile(t, filepath.Join(transaction.StageRoot(), "gen", "changed.go"), "after")
	writeTestFile(t, filepath.Join(transaction.StageRoot(), "gen", "new.go"), "new")
	writeTestFile(t, filepath.Join(transaction.StageRoot(), "proto", "buf.lock"), "new-lock")
	if err := transaction.TrackDirectory("gen"); err != nil {
		t.Fatal(err)
	}
	if err := transaction.TrackFile(filepath.Join("proto", "buf.lock")); err != nil {
		t.Fatal(err)
	}
	if err := transaction.TrackFile(filepath.Join("proto", "obsolete.lock")); err != nil {
		t.Fatal(err)
	}

	changed, err := transaction.ChangedFiles()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"modules/api/gen/changed.go",
		"modules/api/gen/new.go",
		"modules/api/gen/stale.go",
		"modules/api/proto/buf.lock",
		"modules/api/proto/obsolete.lock",
	}
	if !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed files = %#v, want %#v", changed, want)
	}
	assertTestFile(t, filepath.Join(root, "gen", "changed.go"), "before")
	assertTestFile(t, filepath.Join(root, "gen", "stale.go"), "stale")

	if err := transaction.Apply(); err != nil {
		t.Fatal(err)
	}
	assertTestFile(t, filepath.Join(root, "gen", "changed.go"), "after")
	assertTestFile(t, filepath.Join(root, "gen", "new.go"), "new")
	assertTestFile(t, filepath.Join(root, "proto", "buf.lock"), "new-lock")
	assertTestFile(t, filepath.Join(root, "user.go"), "user-owned")
	if _, err := os.Stat(filepath.Join(root, "proto", "obsolete.lock")); !os.IsNotExist(err) {
		t.Fatalf("stale generated file target still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "gen", "stale.go")); !os.IsNotExist(err) {
		t.Fatalf("stale generated file still exists: %v", err)
	}
	after, err := transaction.ChangedFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("applied transaction still reports drift: %v", after)
	}
}

func TestSyncTransactionApplyIsAllOrNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only directories do not block root")
	}
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "gen", "changed.go"), "before")
	writeTestFile(t, filepath.Join(root, "locked", "gen.go"), "locked-before")

	transaction, err := newSyncTransaction(root, "modules/api")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transaction.Close() }()
	writeTestFile(t, filepath.Join(transaction.StageRoot(), "gen", "changed.go"), "after")
	writeTestFile(t, filepath.Join(transaction.StageRoot(), "locked", "gen.go"), "locked-after")
	if err := transaction.TrackDirectory("gen"); err != nil {
		t.Fatal(err)
	}
	if err := transaction.TrackFile(filepath.Join("locked", "gen.go")); err != nil {
		t.Fatal(err)
	}

	// "gen" sorts before "locked", so under a target-by-target apply it would
	// already be replaced when the unwritable target fails.
	lockedDir := filepath.Join(root, "locked")
	if err := os.Chmod(lockedDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedDir, 0o755) })

	if err := transaction.Apply(); err == nil {
		t.Fatal("apply succeeded despite unwritable target")
	}
	assertTestFile(t, filepath.Join(root, "gen", "changed.go"), "before")
	assertTestFile(t, filepath.Join(root, "locked", "gen.go"), "locked-before")

	if err := os.Chmod(lockedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Apply(); err != nil {
		t.Fatal(err)
	}
	assertTestFile(t, filepath.Join(root, "gen", "changed.go"), "after")
	assertTestFile(t, filepath.Join(root, "locked", "gen.go"), "locked-after")
	err = filepath.Walk(root, func(path string, _ os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if strings.Contains(path, ".codefly-sync-") {
			t.Errorf("staging leftover %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRollbackSwapsRestoresCommittedTargetsOnMidCommitFailure(t *testing.T) {
	root := t.TempDir()
	// Two targets whose originals both exist. The first swaps in cleanly; the
	// second fails at commit time (after its original is moved aside) because
	// its staged replacement is missing. This is the path the Apply permission
	// test cannot reach — a failure during a later commit(), not during
	// prepareSwaps — so exercise commit()/rollbackSwaps() directly.
	writeTestFile(t, filepath.Join(root, "first.go"), "first-before")
	writeTestFile(t, filepath.Join(root, "second.go"), "second-before")

	firstActual := filepath.Join(root, "first.go")
	firstIncoming := firstActual + syncIncomingSuffix
	writeTestFile(t, firstIncoming, "first-after")

	secondActual := filepath.Join(root, "second.go")
	secondIncoming := secondActual + syncIncomingSuffix // never materialized: forces commit to fail

	swaps := []*pendingSwap{
		{relative: "first.go", actual: firstActual, incoming: firstIncoming},
		{relative: "second.go", actual: secondActual, incoming: secondIncoming},
	}

	if err := swaps[0].commit(); err != nil {
		t.Fatalf("first commit failed: %v", err)
	}
	assertTestFile(t, firstActual, "first-after")

	if err := swaps[1].commit(); err == nil {
		t.Fatal("second commit succeeded despite missing incoming replacement")
	}

	if err := rollbackSwaps(swaps); err != nil {
		t.Fatalf("rollback failed: %v", err)
	}

	// The committed first target is undone and the second target's displaced
	// original is restored: both are back to their pre-apply content.
	assertTestFile(t, firstActual, "first-before")
	assertTestFile(t, secondActual, "second-before")

	err := filepath.Walk(root, func(path string, _ os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if strings.Contains(path, syncBackupSuffix) {
			t.Errorf("backup leftover %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSyncTransactionRejectsBroadAndOverlappingOwnership(t *testing.T) {
	transaction, err := newSyncTransaction(t.TempDir(), "module/service")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transaction.Close() }()
	if err := transaction.TrackDirectory("."); err == nil {
		t.Fatal("service-root ownership was accepted")
	}
	if err := transaction.TrackDirectory("../outside"); err == nil {
		t.Fatal("escaping ownership was accepted")
	}
	if err := transaction.TrackDirectory("gen"); err != nil {
		t.Fatal(err)
	}
	if err := transaction.TrackFile(filepath.Join("gen", "nested.go")); err == nil {
		t.Fatal("overlapping ownership was accepted")
	}
}

func TestSyncNodesEqualUsesGitStablePermissions(t *testing.T) {
	root := t.TempDir()
	left := filepath.Join(root, "checkout.go")
	right := filepath.Join(root, "generated.go")
	writeTestFile(t, left, "package generated")
	writeTestFile(t, right, "package generated")

	if err := os.Chmod(left, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(right, 0o600); err != nil {
		t.Fatal(err)
	}
	equal, err := syncNodesEqual(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if !equal {
		t.Fatal("non-executable checkout and container permissions reported drift")
	}

	if err := os.Chmod(right, 0o700); err != nil {
		t.Fatal(err)
	}
	equal, err = syncNodesEqual(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if equal {
		t.Fatal("executable permission change was ignored")
	}
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertTestFile(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("%s = %q, want %q", path, body, want)
	}
}
