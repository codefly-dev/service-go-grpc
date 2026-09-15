package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// BuildCommand declares one executable without introducing shell build hooks.
type BuildCommand struct {
	Name    string `yaml:"name"`
	Package string `yaml:"package"`
}

var commandName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
var commandPackage = regexp.MustCompile(`^\./[a-zA-Z0-9_-]+(/[a-zA-Z0-9_-]+)*$`)

func validateBuildCommands(commands []BuildCommand) error {
	names := make(map[string]bool, len(commands))
	for _, command := range commands {
		if !commandName.MatchString(command.Name) || command.Name == "app" {
			return fmt.Errorf("build-commands: invalid executable name %q", command.Name)
		}
		if names[command.Name] {
			return fmt.Errorf("build-commands: duplicate executable name %q", command.Name)
		}
		names[command.Name] = true
		if !commandPackage.MatchString(command.Package) {
			return fmt.Errorf("build-commands: %q must select one ./package below the Go module", command.Name)
		}
	}
	return nil
}

// Refuse missing, external and nested-module packages before emitting a recipe.
// The image build additionally checks that the selected package is executable
// for its actual GOOS/GOARCH; a library must never be copied as a command.
func validateBuildCommandSources(moduleRoot string, commands []BuildCommand) error {
	if err := validateBuildCommands(commands); err != nil {
		return err
	}
	if len(commands) == 0 {
		return nil
	}
	root, err := filepath.EvalSymlinks(moduleRoot)
	if err != nil {
		return fmt.Errorf("build-commands: Go module directory is unavailable")
	}
	for _, command := range commands {
		declared := filepath.Join(root, command.Package)
		resolved, err := filepath.EvalSymlinks(declared)
		if err != nil || resolved != declared {
			return fmt.Errorf("build-commands: %q must name an existing non-symlink package", command.Name)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("build-commands: %q must name a package directory", command.Name)
		}
		for dir := resolved; dir != root; dir = filepath.Dir(dir) {
			if !strings.HasPrefix(dir, root+string(filepath.Separator)) {
				return fmt.Errorf("build-commands: %q escapes the Go module", command.Name)
			}
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil || !os.IsNotExist(err) {
				return fmt.Errorf("build-commands: %q crosses a nested Go module", command.Name)
			}
		}
	}
	return nil
}
