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
