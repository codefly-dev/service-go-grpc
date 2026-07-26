package main

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// stageBufGen writes a buf.gen.yaml into stageRoot/proto and returns the stage root.
func stageBufGen(t *testing.T, body string) string {
	t.Helper()
	stageRoot := t.TempDir()
	protoDir := filepath.Join(stageRoot, "proto")
	if err := os.MkdirAll(protoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(protoDir, "buf.gen.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return stageRoot
}

// pluginOuts reads back every plugin `out` value from the staged buf.gen.yaml.
func pluginOuts(t *testing.T, stageRoot string) []string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(stageRoot, "proto", "buf.gen.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		t.Fatal(err)
	}
	var outs []string
	for _, node := range bufGenOutputNodes(&doc) {
		outs = append(outs, node.Value)
	}
	return outs
}

const bufGenWithCrossService = `version: v2
plugins:
  - local: protoc-gen-go
    out: ../code/pkg/gen
    opt: paths=source_relative
  - local: protoc-gen-openapiv2
    out: ../.cache/openapi
  - local: protoc-gen-es
    out: ../../frontend/code/src/gen
    opt:
      - target=ts
`

func TestRedirectEscapingBufOutputs_RedirectsOnlyEscapingOutputs(t *testing.T) {
	stageRoot := stageBufGen(t, bufGenWithCrossService)

	if err := redirectEscapingBufOutputs(stageRoot, "proto"); err != nil {
		t.Fatalf("redirect: %v", err)
	}

	outs := pluginOuts(t, stageRoot)
	want := []string{
		"../code/pkg/gen",                     // in-tree: unchanged
		"../.cache/openapi",                   // in-tree (buf's private cache): unchanged
		"../" + crossServiceDiscardDir + "/0", // escaping: redirected into the stage
	}
	if len(outs) != len(want) {
		t.Fatalf("got %d outputs %v, want %d %v", len(outs), outs, len(want), want)
	}
	for i := range want {
		if outs[i] != want[i] {
			t.Errorf("output %d = %q, want %q", i, outs[i], want[i])
		}
	}

	// Every redirected output must resolve back inside the stage so the
	// containerized generator can write it under any container user.
	redirected := filepath.Clean(filepath.Join(stageRoot, "proto", outs[2]))
	if !pathWithin(stageRoot, redirected) {
		t.Errorf("redirected output %q escapes stage %q", redirected, stageRoot)
	}
}

func TestRedirectEscapingBufOutputs_NoEscapeLeavesFileUntouched(t *testing.T) {
	body := "version: v2\nplugins:\n  - local: protoc-gen-go\n    out: ../code/pkg/gen\n"
	stageRoot := stageBufGen(t, body)
	original, _ := os.ReadFile(filepath.Join(stageRoot, "proto", "buf.gen.yaml"))

	if err := redirectEscapingBufOutputs(stageRoot, "proto"); err != nil {
		t.Fatalf("redirect: %v", err)
	}

	rewritten, _ := os.ReadFile(filepath.Join(stageRoot, "proto", "buf.gen.yaml"))
	if string(original) != string(rewritten) {
		t.Errorf("buf.gen.yaml with no escaping output was rewritten:\n%s", rewritten)
	}
}

func TestRedirectEscapingBufOutputs_MissingFileIsNoError(t *testing.T) {
	if err := redirectEscapingBufOutputs(t.TempDir(), "proto"); err != nil {
		t.Fatalf("missing buf.gen.yaml should be tolerated, got: %v", err)
	}
}
