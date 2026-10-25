package main

import (
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/module"
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
	seen := map[string]string{}
	for i, p := range file.Plugins {
		if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Package) == "" {
			return nil, fmt.Errorf("plugins/plugins.yaml: entry %d needs a name and a package", i+1)
		}
		if err := module.CheckImportPath(p.Package); err != nil {
			return nil, fmt.Errorf("plugins/plugins.yaml: plugin %q: package is not a Go import path: %w", p.Name, err)
		}
		alias := p.Alias()
		if token.IsKeyword(alias) || reservedAliases[alias] {
			return nil, fmt.Errorf("plugins/plugins.yaml: plugin %q: its import alias %q is reserved in the generated registry; rename the plugin", p.Name, alias)
		}
		if first, taken := seen[alias]; taken {
			if first == p.Name {
				return nil, fmt.Errorf("plugins/plugins.yaml: plugin %q is listed twice", p.Name)
			}
			return nil, fmt.Errorf("plugins/plugins.yaml: plugins %q and %q both import as %q; rename one", first, p.Name, alias)
		}
		seen[alias] = p.Name
	}
	return file.Plugins, nil
}

// reservedAliases are the identifiers the generated registry already uses at
// file scope, which a plugin's import alias would collide with.
var reservedAliases = map[string]bool{"framework": true, "All": true, "_": true}

// pluginsNeedTheScaffold refuses a service that lists plugins when Sync does
// not regenerate its scaffold. The registry is part of that scaffold (it
// imports the scaffold's framework package), so rendering it alone would not
// compile, and skipping it would drop every listed plugin without a word.
func pluginsNeedTheScaffold(plugins []PluginEntry, scaffoldTargets []string, notRegenerated string) error {
	if len(plugins) == 0 || len(scaffoldTargets) > 0 {
		return nil
	}
	return fmt.Errorf("plugins/plugins.yaml lists %d plugin(s), but the plugin registry is not rendered: %s", len(plugins), notRegenerated)
}
