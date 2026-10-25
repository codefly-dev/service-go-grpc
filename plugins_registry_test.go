package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

func TestTheRegistryIsRenderedFromPluginsYAMLInOrder(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugins", "plugins.yaml"), []byte("plugins:\n  - name: authority\n    package: gateway/plugins/authority\n  - name: org-directory\n    package: gateway/plugins/orgdirectory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plugins, err := readPlugins(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 2 || plugins[1].Alias() != "org_directory" {
		t.Fatalf("plugins = %+v", plugins)
	}
	raw, err := factoryFS.ReadFile("templates/factory/code/plugins/registry_gen.go.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	tmpl := template.Must(template.New("registry").Parse(string(raw)))
	var out strings.Builder
	data := map[string]any{"Service": map[string]any{"Name": map[string]any{"DNSCase": "gateway"}}, "Plugins": plugins}
	if err := tmpl.Execute(&out, data); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`authority "gateway/plugins/authority"`, `org_directory "gateway/plugins/orgdirectory"`, "authority.New(),", "org_directory.New(),"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("registry lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Index(out.String(), "authority.New()") > strings.Index(out.String(), "org_directory.New()") {
		t.Fatal("plugins are registered out of order")
	}
}

func TestAnEmptyOrMissingRegistryRendersNil(t *testing.T) {
	plugins, err := readPlugins(t.TempDir())
	if err != nil || plugins != nil {
		t.Fatalf("missing file: %v %v", plugins, err)
	}
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "plugins"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "plugins", "plugins.yaml"), []byte("plugins:\n  - name: broken\n"), 0o644)
	if _, err := readPlugins(root); err == nil {
		t.Fatal("an entry without a package is refused by name")
	}
}
