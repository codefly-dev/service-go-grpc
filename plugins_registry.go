package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// PluginEntry is one active plugin a service lists in code/plugins/plugins.yaml:
// a package exposing `func New() framework.Plugin`. The registry the agent
// renders imports each package and returns one instance of each, in order.
type PluginEntry struct {
	Name    string `yaml:"name"`
	Package string `yaml:"package"`
}

// Alias is the import alias the registry uses for this plugin's package: its
// name as a Go identifier.
func (p PluginEntry) Alias() string {
	alias := nonIdentifier.ReplaceAllString(p.Name, "_")
	if alias == "" || (alias[0] >= '0' && alias[0] <= '9') {
		alias = "plugin_" + alias
	}
	return alias
}

var nonIdentifier = regexp.MustCompile(`[^A-Za-z0-9_]`)

type pluginsFile struct {
	Plugins []PluginEntry `yaml:"plugins"`
}

// readPlugins reads the service's plugin registry declaration. A missing file is
// an empty registry; a malformed one, or an entry without a name or a package, is
// an error named by entry so that a listed plugin is never silently dropped.
func readPlugins(codeRoot string) ([]PluginEntry, error) {
	raw, err := os.ReadFile(filepath.Join(codeRoot, "plugins", "plugins.yaml"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var file pluginsFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("plugins/plugins.yaml: %w", err)
	}
	seen := map[string]bool{}
	for i, p := range file.Plugins {
		if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Package) == "" {
			return nil, fmt.Errorf("plugins/plugins.yaml: entry %d needs a name and a package", i+1)
		}
		if seen[p.Alias()] {
			return nil, fmt.Errorf("plugins/plugins.yaml: plugin %q is listed twice", p.Name)
		}
		seen[p.Alias()] = true
	}
	return file.Plugins, nil
}
