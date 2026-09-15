package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// A declared runtime path must reach the final image as ordinary files. Docker
// can otherwise preserve a dangling symlink or omit the sibling data the binary
// reads after startup. Validate before returning a successful build recipe.
func validateRuntimeAssetSources(root string, assets []string) error {
	for _, asset := range assets {
		if err := validateRuntimeAssetPath(asset); err != nil {
			return err
		}
		path := filepath.Join(root, asset)
		resolved, err := filepath.EvalSymlinks(path)
		resolvedRoot, rootErr := filepath.EvalSymlinks(root)
		if err != nil || rootErr != nil || resolved != filepath.Join(resolvedRoot, asset) {
			return fmt.Errorf("runtime asset %q must exist without symlinks", asset)
		}
		if err := filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 || (!entry.IsDir() && !entry.Type().IsRegular()) {
				return fmt.Errorf("runtime asset %q contains a link or special file", asset)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("runtime asset %q cannot be staged as ordinary files", asset)
		}
	}
	return nil
}
