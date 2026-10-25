package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A full sync replaces generated directories. Every declared input must reach
// the stage or bindings for a sibling contract disappear on regeneration.
func TestSyncStagesBufDirectoryInputsWithoutChangingTheTemplate(t *testing.T) {
	root := t.TempDir()
	template := "version: v2\ninputs:\n  - directory: .\n    paths: [api.proto]\n  - directory: ../proto-client\nplugins: []\n"
	writeTestFile(t, filepath.Join(root, "proto", "buf.gen.yaml"), template)
	writeTestFile(t, filepath.Join(root, "proto-client", "api.proto"), "client contract")
	tx, err := newSyncTransaction(root, "service")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Close() })
	if err := tx.CopyInput("proto"); err != nil {
		t.Fatal(err)
	}
	if err := stageBufDirectoryInputs(tx, "proto"); err != nil {
		t.Fatal(err)
	}
	assertTestFile(t, filepath.Join(tx.StageRoot(), "proto-client", "api.proto"), "client contract")
	assertTestFile(t, filepath.Join(tx.StageRoot(), "proto", "buf.gen.yaml"), template)
	assertTestFile(t, filepath.Join(root, "proto-client", "api.proto"), "client contract")
}

func TestSyncRefusesBufInputThroughEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "proto", "buf.gen.yaml"), "version: v2\ninputs:\n  - directory: ../linked\n")
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	tx, err := newSyncTransaction(root, "service")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Close() })
	if err := tx.CopyInput("proto"); err != nil {
		t.Fatal(err)
	}
	if err := stageBufDirectoryInputs(tx, "proto"); err == nil {
		t.Fatal("accepted an input redirected outside the service")
	}
}

// A template cannot use staging to read a host path outside its service. Missing
// local inputs also fail before replacing any generated output.
func TestSyncRefusesUnavailableBufDirectoryInputs(t *testing.T) {
	for _, input := range []string{"../../outside", "/outside", "../missing"} {
		t.Run(input, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, filepath.Join(root, "proto", "buf.gen.yaml"), "version: v2\ninputs:\n  - directory: "+input+"\n")
			tx, err := newSyncTransaction(root, "service")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tx.Close() })
			if err := tx.CopyInput("proto"); err != nil {
				t.Fatal(err)
			}
			if err := stageBufDirectoryInputs(tx, "proto"); err == nil {
				t.Fatal("accepted an unavailable directory input")
			}
		})
	}
}

// Core's proto companion reads the go.mod governing a Go output root to decide
// which vendored protos are foreign; the stage must carry that go.mod, and
// only that one, or a sync emits bindings the service already imports.
func TestSyncStagesTheGoModGoverningEachOutputRoot(t *testing.T) {
	root := t.TempDir()
	goMod := "module records\n\nrequire github.com/codefly-dev/authority/libraries/hop/go v0.0.0\n"
	writeTestFile(t, filepath.Join(root, "code", "go.mod"), goMod)
	if err := os.MkdirAll(filepath.Join(root, "code", "pkg", "gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	tx, err := newSyncTransaction(root, "service")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Close() })
	if err := stageGoverningGoModules(tx, []string{"code/pkg/gen", "openapi"}); err != nil {
		t.Fatal(err)
	}
	assertTestFile(t, filepath.Join(tx.StageRoot(), "code", "go.mod"), goMod)
	if _, err := os.Lstat(filepath.Join(tx.StageRoot(), "go.mod")); !os.IsNotExist(err) {
		t.Fatalf("an output root with no governing go.mod staged one: %v", err)
	}
	if len(tx.targets) != 0 {
		t.Fatalf("a governing go.mod is input, never a sync target: %v", tx.targets)
	}
}
