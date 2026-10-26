package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/modfile"
)

// moduleScopedTemplates are the factory templates that name the service's own
// Go module, with the path each must render under it.
var moduleScopedTemplates = map[string]string{
	"templates/factory/code/go.mod.tmpl":                            "module %s\n",
	"templates/factory/code/main.go.tmpl":                           `"%s/pkg/adapters"`,
	"templates/factory/code/plugins/registry_gen.go.tmpl":           `"%s/pkg/framework"`,
	"templates/factory/code/pkg/adapters/rest_gen.go.tmpl":          `"%s/plugins"`,
	"templates/factory/code/pkg/adapters/server_gen.go.tmpl":        `"%s/plugins"`,
	"templates/factory/code/pkg/adapters/grpc_gen.go.tmpl":          `gen "%s/pkg/gen"`,
	"templates/factory/code/pkg/gen/genconnect/api.connect.go.tmpl": `gen "%s/pkg/gen"`,
	"templates/factory/proto/api.proto.tmpl":                        `option go_package = "%s/pkg/gen;gen";`,
}

// The templates once rendered the service name as the Go module (`module
// chat`), so a service whose go.mod declares a fetchable path had every
// regenerated import rewritten to one Go cannot resolve, and no other module
// could import it. An existing go.mod's path must root every rendered import;
// only a service with no go.mod falls back to its name.
func TestFactoryTemplatesRootImportsAtTheExistingGoModule(t *testing.T) {
	for _, tc := range []struct{ name, existing, want string }{
		{"existing go.mod", "github.com/x/y/services/z/code", "github.com/x/y/services/z/code"},
		{"new service", "", "z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			create := CreateConfiguration{
				Information:  &services.Information{Service: &resources.ServiceWithCase{Name: shared.Case{DNSCase: "z", Title: "Z", CamelCase: "z"}}},
				Settings:     &Settings{RestEndpoint: true, ConnectEndpoint: true},
				GoModulePath: tc.existing,
			}
			for path, shape := range moduleScopedTemplates {
				raw, err := factoryFS.ReadFile(path)
				require.NoError(t, err)
				rendered, err := templates.ApplyTemplate(string(raw), create)
				require.NoError(t, err, path)
				want := strings.Replace(shape, "%s", tc.want, 1)
				require.Contains(t, rendered, want, path)
				if tc.existing != "" {
					require.NotContains(t, rendered, `"z/`, "%s still roots an import at the service name", path)
				}
			}
		})
	}
}

func TestExistingGoModuleReadsTheDeclaredPath(t *testing.T) {
	root := t.TempDir()
	got, err := existingGoModule(filepath.Join(root, "go.mod"))
	require.NoError(t, err)
	require.Empty(t, got, "no go.mod means a new service")
	writeTestFile(t, filepath.Join(root, "go.mod"), "module github.com/x/y/services/z/code\n\ngo 1.27\n")
	got, err = existingGoModule(filepath.Join(root, "go.mod"))
	require.NoError(t, err)
	require.Equal(t, "github.com/x/y/services/z/code", got)
}

// End to end through the real Builder: Create (which renders the factory and
// then runs Sync, Buf included) and a later Sync keep an existing go.mod's
// module path, and every import the agent generated is rooted at it. A service
// created with no go.mod still gets its name, as before.
func TestCreateAndSyncKeepTheServicesGoModulePath(t *testing.T) {
	protoCompanionMu.Lock()
	defer protoCompanionMu.Unlock()
	for _, tc := range []struct{ name, existing, want string }{
		{"existing go.mod", "github.com/x/y/services/svc/code", "github.com/x/y/services/svc/code"},
		{"new service", "", "svc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root := t.TempDir()
			serviceDir := filepath.Join(root, "mod", "svc")
			service := &resources.Service{Name: "svc", Version: "0.0.0"}
			require.NoError(t, service.SaveAtDir(ctx, serviceDir))
			require.NoError(t, (&resources.Module{Name: "mod"}).SaveToDir(ctx, filepath.Join(root, "mod")))
			if tc.existing != "" {
				writeTestFile(t, filepath.Join(serviceDir, "code", "go.mod"), "module "+tc.existing+"\n\ngo 1.27\n")
			}
			builder := NewBuilder(NewService())
			_, err := builder.Load(ctx, &builderv0.LoadRequest{
				Identity: &basev0.ServiceIdentity{
					Name: "svc", Version: "0.0.0", Module: "mod", Workspace: "test",
					WorkspacePath: root, RelativeToWorkspace: "mod/svc",
				},
				CreationMode: &builderv0.CreationMode{},
			})
			require.NoError(t, err)
			created, err := builder.Create(ctx, &builderv0.CreateRequest{})
			require.NoError(t, err)
			require.Equal(t, builderv0.CreateStatus_CREATED, created.GetState().GetState(), created.GetState().GetMessage())
			requireImportsRootedAt(t, serviceDir, tc.want)

			response, err := builder.Sync(ctx, &builderv0.SyncRequest{})
			require.NoError(t, err)
			require.Equal(t, builderv0.SyncStatus_SUCCESS, response.GetState().GetState(), response.GetState().GetMessage())
			requireImportsRootedAt(t, serviceDir, tc.want)
			if tc.existing != "" {
				return
			}

			// A service created under its name whose author then gives go.mod
			// (and the proto it owns) a fetchable path: Sync alone, with no
			// Create, re-roots the scaffold it regenerates at that path.
			const moved = "github.com/x/y/services/svc/code"
			goMod := filepath.Join(serviceDir, "code", "go.mod")
			replaceInFile(t, goMod, "module svc\n", "module "+moved+"\n")
			replaceInFile(t, filepath.Join(serviceDir, "proto", "api.proto"), `"svc/pkg/gen;gen"`, `"`+moved+`/pkg/gen;gen"`)
			response, err = builder.Sync(ctx, &builderv0.SyncRequest{})
			require.NoError(t, err)
			require.Equal(t, builderv0.SyncStatus_SUCCESS, response.GetState().GetState(), response.GetState().GetMessage())
			requireImportsRootedAt(t, serviceDir, moved)
		})
	}
}

// requireImportsRootedAt checks go.mod, the proto's go_package and every
// import of the service's own packages in the generated Go tree.
func requireImportsRootedAt(t *testing.T, serviceDir, module string) {
	t.Helper()
	mod, err := os.ReadFile(filepath.Join(serviceDir, "code", "go.mod"))
	require.NoError(t, err)
	require.Equal(t, module, modfile.ModulePath(mod))
	proto, err := os.ReadFile(filepath.Join(serviceDir, "proto", "api.proto"))
	require.NoError(t, err)
	require.Contains(t, string(proto), `option go_package = "`+module+`/pkg/gen;gen";`)

	own := map[string]bool{}
	fset := token.NewFileSet()
	require.NoError(t, filepath.WalkDir(filepath.Join(serviceDir, "code"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".go" {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			value, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(value, "svc/") && module != "svc" {
				t.Errorf("%s imports %q, rooted at the service name instead of %s", path, value, module)
			}
			if strings.HasPrefix(value, module+"/") {
				own[value] = true
			}
		}
		return nil
	}))
	for _, want := range []string{"/pkg/adapters", "/pkg/framework", "/pkg/gen", "/plugins"} {
		require.True(t, own[module+want], "no generated file imports %s%s (own imports: %v)", module, want, own)
	}
}

func replaceInFile(t *testing.T, path, old, new string) {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(content), old, path)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(content), old, new, 1)), 0o644))
}
