package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// stageBufDirectoryInputs preserves local directory inputs declared by a Buf
// v2 generation template. Sync replaces the generated output trees, so staging
// only the primary protocol directory loses bindings from sibling inputs.
// Buf still owns input selection (including paths and excludes); this function
// only makes its declared local directories available in the transaction.
func stageBufDirectoryInputs(transaction *syncTransaction, protoDir string) error {
	template := filepath.Join(transaction.StageRoot(), protoDir, "buf.gen.yaml")
	contents, err := os.ReadFile(template)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read generation inputs: %w", err)
	}
	var config struct {
		Inputs []struct {
			Directory string `yaml:"directory"`
		} `yaml:"inputs"`
	}
	if err := yaml.Unmarshal(contents, &config); err != nil {
		return fmt.Errorf("parse generation inputs: %w", err)
	}
	staged := map[string]bool{}
	root, err := filepath.EvalSymlinks(transaction.actualRoot)
	if err != nil {
		return fmt.Errorf("resolve generation input boundary: %w", err)
	}
	for _, input := range config.Inputs {
		if input.Directory == "" {
			continue // Non-directory inputs remain Buf's responsibility.
		}
		if filepath.IsAbs(input.Directory) {
			return fmt.Errorf("generation directory input %q must be relative to its template and stay inside the service", input.Directory)
		}
		relative, err := cleanSyncRelative(filepath.Join(protoDir, input.Directory))
		if err != nil {
			return fmt.Errorf("generation directory input %q: %w", input.Directory, err)
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(transaction.actualRoot, relative))
		if err != nil {
			return fmt.Errorf("resolve generation directory input %q: %w", input.Directory, err)
		}
		if !pathWithin(root, resolved) {
			return fmt.Errorf("generation directory input %q resolves outside the service", input.Directory)
		}
		if relative == protoDir || pathContains(protoDir, relative) || staged[relative] {
			continue
		}
		if err := transaction.CopyInput(relative); err != nil {
			return fmt.Errorf("stage generation directory input %q: %w", input.Directory, err)
		}
		staged[relative] = true
	}
	return nil
}

// stageGoverningGoModules stages, as read-only input, the go.mod that governs
// each declared output root: the nearest one at or above it inside the
// service. Core's proto companion calls a vendored proto foreign to Go when
// its go_package lies in a module that go.mod requires, and removes the
// bindings buf wrote for it; it reads the go.mod governing the root it
// generated into, which is this stage. Without the go.mod there the companion
// sees no requirement, and a sync writes a second registration of every
// vendored proto whose Go package the service already imports.
func stageGoverningGoModules(transaction *syncTransaction, outputDirs []string) error {
	staged := map[string]bool{}
	for _, output := range outputDirs {
		relative, err := cleanSyncRelative(output)
		if err != nil {
			return fmt.Errorf("generated output %q: %w", output, err)
		}
		for dir := relative; ; dir = filepath.Dir(dir) {
			goMod := filepath.Join(dir, "go.mod")
			info, err := os.Lstat(filepath.Join(transaction.actualRoot, goMod))
			switch {
			case err == nil && info.Mode().IsRegular():
				if !staged[goMod] {
					if err := transaction.CopyInput(goMod); err != nil {
						return err
					}
					staged[goMod] = true
				}
			case err != nil && !os.IsNotExist(err):
				return fmt.Errorf("stat %q: %w", goMod, err)
			case dir != ".":
				continue
			}
			break
		}
	}
	return nil
}
