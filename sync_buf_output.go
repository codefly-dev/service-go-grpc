package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// crossServiceDiscardDir is the throwaway, untracked directory inside the sync
// stage that receives generator output aimed outside the staged service tree.
const crossServiceDiscardDir = ".codefly-cross-service"

// redirectEscapingBufOutputs rewrites `out` directories in the staged
// buf.gen.yaml that resolve outside the staged service tree so they land in a
// throwaway inside the stage.
//
// A service that owns a proto may cross-write a consumer's client — the SaaS
// accounts service emits its TypeScript client into a sibling frontend via
// `out: ../../frontend/code/src/gen`. Sync stages only this service, and the
// companion mounts that stage near the container root, so such a path escapes
// the mount to a container-absolute directory (`/frontend`) that the non-root
// host user the companion runs as on Linux cannot create — buf then dies with
// `mkdir /frontend: permission denied`. On macOS the companion runs as root and
// silently writes the same output into the throwaway container filesystem, so
// the failure is Linux-only and easy to miss.
//
// A cross-service output is never a sync target — cleanSyncRelative rejects
// non-local paths, so ChangedFiles/Apply only ever consider paths inside the
// service — meaning this output is discarded no matter where it lands.
// Redirecting it into the stage keeps every tracked output byte-identical while
// letting the containerized generation succeed for any container user.
func redirectEscapingBufOutputs(stageRoot, protoDir string) error {
	bufGen := filepath.Join(stageRoot, protoDir, "buf.gen.yaml")
	content, err := os.ReadFile(bufGen)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read staged buf.gen.yaml: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return fmt.Errorf("parse staged buf.gen.yaml: %w", err)
	}

	bufRoot := filepath.Join(stageRoot, protoDir)
	redirected := 0
	for _, out := range bufGenOutputNodes(&doc) {
		resolved := filepath.Clean(filepath.Join(bufRoot, out.Value))
		if pathWithin(stageRoot, resolved) {
			continue
		}
		target := filepath.Join(stageRoot, crossServiceDiscardDir, fmt.Sprintf("%d", redirected))
		relative, err := filepath.Rel(bufRoot, target)
		if err != nil {
			return fmt.Errorf("scope cross-service output %q: %w", out.Value, err)
		}
		out.Value = filepath.ToSlash(relative)
		redirected++
	}
	if redirected == 0 {
		return nil
	}

	rewritten, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("rewrite staged buf.gen.yaml: %w", err)
	}
	if err := os.WriteFile(bufGen, rewritten, 0o644); err != nil {
		return fmt.Errorf("write staged buf.gen.yaml: %w", err)
	}
	return nil
}

// bufGenOutputNodes returns the scalar value nodes of every plugin `out:` entry
// in a buf.gen.yaml document, so a caller can inspect and rewrite them in place.
func bufGenOutputNodes(doc *yaml.Node) []*yaml.Node {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	var plugins *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "plugins" {
			plugins = root.Content[i+1]
			break
		}
	}
	if plugins == nil || plugins.Kind != yaml.SequenceNode {
		return nil
	}
	var outs []*yaml.Node
	for _, plugin := range plugins.Content {
		if plugin.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(plugin.Content); i += 2 {
			if plugin.Content[i].Value == "out" && plugin.Content[i+1].Kind == yaml.ScalarNode {
				outs = append(outs, plugin.Content[i+1])
			}
		}
	}
	return outs
}

// pathWithin reports whether path is root itself or a descendant of it.
func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
