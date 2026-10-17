package main

import (
	"os"
	"testing"

	"golang.org/x/mod/modfile"
)

// corePath is the module whose pin this agent and every service it scaffolds
// must agree on.
const corePath = "github.com/codefly-dev/core"

// TestTheCorePinIsAReleaseNotAReplace: `go.mod` must reach core through its
// `require` line alone.
//
// A `replace` is invisible to the dependency-lock gate — TestFactoryDependencyLocksMatchBase
// compares require lines, so this module and `base/code` can report the same
// core version while this one actually compiles against something else
// entirely. That is a hole with teeth: GoReleaser builds from the `v*` tag, so
// a release merged while a replace stands publishes an agent whose module graph
// points somewhere other than core — a fork, a personal account, an unmerged
// branch — and nothing downstream can tell.
//
// A replace is therefore allowed only in a working tree, never on a branch that
// can be merged. When a needed core change is not yet released, the deliverable
// is the core release: land the core PR, tag it, move the `require` line here
// and in `base/code/go.mod` plus the two factory templates, and drop the
// replace. This test is what makes that the only way out.
func TestTheCorePinIsAReleaseNotAReplace(t *testing.T) {
	for _, path := range []string{"go.mod", "base/code/go.mod"} {
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		parsed, err := modfile.Parse(path, payload, nil)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, replacement := range parsed.Replace {
			if replacement.Old.Path != corePath {
				continue
			}
			t.Fatalf("%s replaces %s with %s %s: an agent that can be merged pins a core RELEASE. "+
				"Land the core change, tag it, move the require line in go.mod, base/code/go.mod and "+
				"templates/factory/code/go.{mod,sum}.tmpl, and drop the replace.",
				path, corePath, replacement.New.Path, replacement.New.Version)
		}
	}
}
