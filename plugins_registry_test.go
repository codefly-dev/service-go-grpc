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

// review: an alias that is a Go keyword or an identifier the registry already
// declares, or a package that is not an import path, wrote a generated
// registry that does not compile (or, through a quote, Go the author never
// wrote). Each is refused by name, as is two names that import as one alias.
func TestPluginEntriesThatCannotCompileAreRefusedByName(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"keyword alias", "plugins:\n  - name: type\n    package: svc/plugins/kind\n", `"type" is reserved`},
		{"framework alias", "plugins:\n  - name: framework\n    package: svc/plugins/framework\n", `"framework" is reserved`},
		{"All alias", "plugins:\n  - name: All\n    package: svc/plugins/all\n", `"All" is reserved`},
		{"quoted package", "plugins:\n  - name: evil\n    package: \"svc/x\\\"\\nfunc init() {}\\n\"\n", "not a Go import path"},
		{"spaced package", "plugins:\n  - name: spaced\n    package: svc/has space\n", "not a Go import path"},
		{"alias collision", "plugins:\n  - name: a.b\n    package: svc/plugins/ab\n  - name: a_b\n    package: svc/plugins/a_b\n", `"a.b" and "a_b" both import as "a_b"`},
		{"listed twice", "plugins:\n  - name: one\n    package: svc/plugins/one\n  - name: one\n    package: svc/plugins/one\n", `"one" is listed twice`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, filepath.Join(root, "plugins", "plugins.yaml"), tc.yaml)
			_, err := readPlugins(root)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("readPlugins = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// review: the registry rendered only when the scaffold did, so a service whose
// scaffold was not regenerated had its listed plugins dropped without a word.
// Sync refuses instead, naming why the scaffold was not regenerated.
func TestListedPluginsWithoutARegeneratedScaffoldAreRefusedByName(t *testing.T) {
	root := t.TempDir()
	protoRoot := filepath.Join(root, "proto")
	writeTestFile(t, filepath.Join(protoRoot, "api.proto"), "syntax = \"proto3\"; service WidgetService {}\n")
	writeTestFile(t, filepath.Join(root, "code", "main.go"), "package main\n")
	targets, why, err := generatedScaffoldTargets(root, protoRoot, "WidgetService")
	if err != nil || len(targets) != 0 {
		t.Fatalf("a hand-written main claimed the scaffold: %v, %v", targets, err)
	}
	listed := []PluginEntry{{Name: "authority", Package: "svc/plugins/authority"}}
	err = pluginsNeedTheScaffold(listed, targets, why)
	if err == nil || !strings.Contains(err.Error(), "hand-written") {
		t.Fatalf("listed plugins over a hand-written main = %v, want a refusal naming the hand-written main", err)
	}
	if err := pluginsNeedTheScaffold(nil, targets, why); err != nil {
		t.Fatalf("no listed plugins needs no scaffold: %v", err)
	}
	if err := pluginsNeedTheScaffold(listed, []string{"code/main.go"}, ""); err != nil {
		t.Fatalf("a regenerated scaffold renders the registry: %v", err)
	}
}
