package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

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
	defer transaction.Close()
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

func TestSyncTransactionRejectsBroadAndOverlappingOwnership(t *testing.T) {
	transaction, err := newSyncTransaction(t.TempDir(), "module/service")
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Close()
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
