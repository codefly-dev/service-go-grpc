package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

func TestBuildCommandsRejectUnsafeDeclarations(t *testing.T) {
	for _, command := range []BuildCommand{
		{Name: "", Package: "./cmd/tool"},
		{Name: "app", Package: "./cmd/tool"},
		{Name: "../tool", Package: "./cmd/tool"},
		{Name: "tool;id", Package: "./cmd/tool"},
		{Name: "tool", Package: "../tool"},
		{Name: "tool", Package: "./cmd/../tool"},
		{Name: "tool", Package: "./cmd/..."},
		{Name: "tool", Package: "./cmd/tool;id"},
		{Name: "tool", Package: "example.com/tool"},
		{Name: "tool", Package: "-o /tmp/tool"},
	} {
		t.Run(command.Name+":"+command.Package, func(t *testing.T) {
			require.Error(t, validateBuildCommands([]BuildCommand{command}))
		})
	}
	valid := BuildCommand{Name: "catalog-import", Package: "./cmd/catalog-import"}
	require.NoError(t, validateBuildCommands([]BuildCommand{valid}))
	require.Error(t, validateBuildCommands([]BuildCommand{valid, valid}))
}

func TestBuildCommandsRejectMissingSymlinkAndNestedModule(t *testing.T) {
	root := t.TempDir()
	command := BuildCommand{Name: "tool", Package: "./cmd/tool"}
	require.Error(t, validateBuildCommandSources(root, []BuildCommand{command}))
	dir := filepath.Join(root, "cmd", "tool")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, validateBuildCommandSources(root, []BuildCommand{command}))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module nested\n"), 0o644))
	require.Error(t, validateBuildCommandSources(root, []BuildCommand{command}))
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(root, "outside")))
	require.Error(t, validateBuildCommandSources(root, []BuildCommand{{Name: "tool", Package: "./outside"}}))
}

// Exercise the same RPC that the CLI uses, including plan verification and both
// final-image copies. This proves recipe generation, not an image execution.
func TestBuildRecipePackagesDeclaredCommandsOverGRPC(t *testing.T) {
	identity := goGrpcServiceFixture(t)
	client, builder := startBuilderAgent(t, identity)
	root := filepath.Join(identity.GetWorkspacePath(), "mod", "svc")
	commands := []BuildCommand{
		{Name: "catalog-import", Package: "./cmd/catalog-import"},
		{Name: "catalog-check", Package: "./cmd/catalog-check"},
	}
	for _, command := range commands {
		dir := filepath.Join(root, "code", command.Package)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644))
	}
	builder.GoGrpc.Settings.BuildCommands = commands
	request := &builderv0.BuildRequest{
		OutputDirectory: filepath.Join(root, "builder"),
		BuildContext: &builderv0.BuildContext{Kind: &builderv0.BuildContext_DockerBuildContext{
			DockerBuildContext: &builderv0.DockerBuildContext{DockerRepository: "registry.example.com"},
		}},
	}
	for _, cgo := range []bool{false, true} {
		builder.GoGrpc.Settings.WithCGO = cgo
		response, err := client.Build(context.Background(), request)
		require.NoError(t, err)
		require.Equal(t, builderv0.BuildStatus_SUCCESS, response.GetState().GetState(), response.GetState().GetMessage())
		require.NoError(t, services.VerifyDockerBuildPlan(request.OutputDirectory, response.GetResult().GetDockerBuildPlan()))
		content, err := os.ReadFile(filepath.Join(request.OutputDirectory, "Dockerfile"))
		require.NoError(t, err)
		for _, command := range commands {
			require.Contains(t, string(content), "-o /app/commands/"+command.Name+" "+command.Package)
			require.Contains(t, string(content), "COPY --chown=appuser --from=builder /app/commands/"+command.Name+" /usr/local/bin/"+command.Name)
			require.Contains(t, string(content), "go list -mod=readonly -f '{{.Name}}' "+command.Package)
		}
		require.Contains(t, string(content), `CMD ["./app"]`)
		if cgo {
			require.Contains(t, string(content), "ENV CGO_ENABLED=1")
			require.NotContains(t, string(content), "-extldflags")
		} else {
			require.Contains(t, string(content), "ENV CGO_ENABLED=0")
			require.Contains(t, string(content), `-extldflags "-static"`)
		}
	}
	builder.GoGrpc.Settings.BuildCommands = []BuildCommand{{Name: "missing", Package: "./cmd/missing"}}
	response, err := client.Build(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_ERROR, response.GetState().GetState())
	require.Nil(t, response.GetResult().GetDockerBuildPlan())
}
