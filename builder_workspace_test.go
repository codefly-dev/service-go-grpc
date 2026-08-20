package main

import (
	"os"
	"path/filepath"
	"testing"

	golanghelpers "github.com/codefly-dev/core/runners/golang"
	"github.com/stretchr/testify/require"
)

func TestGoDockerTemplatingResolvesSymlinkedModuleToRealPath(t *testing.T) {
	// The canonical saas-starter checkout keeps its module at module/ and
	// exposes it to a `layout: modules` workspace through a symlink
	// modules/<name> -> ../module. The build context tars the real tree, so
	// the in-image path must target module/, not the symlink.
	workspace := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "module", "services", "accounts", "code"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "modules"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("..", "module"), filepath.Join(workspace, "modules", "saas-starter")))

	settings := &Settings{}
	settings.SourceDir = "code"
	settings.WithWorkspace = true

	symlinkService := filepath.Join(workspace, "modules", "saas-starter", "services", "accounts")
	configure, _, err := goDockerTemplating(settings, workspace, symlinkService)
	require.NoError(t, err)
	var docker golanghelpers.DockerTemplating
	configure(&docker)

	require.True(t, docker.Workspace)
	require.Equal(t, "module/services/accounts/code", docker.ModuleRoot)
	require.Equal(t, "module/services/accounts/code", docker.SourceDir)
}

func TestGoDockerTemplatingUsesWorkspaceForLocalModuleReplacements(t *testing.T) {
	workspace := t.TempDir()
	service := filepath.Join(workspace, "modules", "users", "services", "forge-edge")
	settings := &Settings{}
	settings.SourceDir = "code/cmd/server"
	settings.WithWorkspace = true
	settings.WithCGO = true

	configure, _, err := goDockerTemplating(settings, workspace, service)
	require.NoError(t, err)
	var docker golanghelpers.DockerTemplating
	configure(&docker)

	require.True(t, docker.Workspace)
	require.Equal(t, workspace, docker.ContextRoot)
	require.Equal(t, "modules/users/services/forge-edge/code", docker.ModuleRoot)
	require.Equal(t, "./cmd/server", docker.BuildTarget)
	require.True(t, docker.WithCGO)
}

func TestGoDockerTemplatingPreservesStandaloneServiceContext(t *testing.T) {
	settings := &Settings{}
	settings.SourceDir = "code/cmd/server"

	configure, _, err := goDockerTemplating(settings, "/workspace", "/workspace/modules/users/services/accounts")
	require.NoError(t, err)
	var docker golanghelpers.DockerTemplating
	configure(&docker)

	require.False(t, docker.Workspace)
	require.Empty(t, docker.ContextRoot)
	require.Equal(t, "code", docker.ModuleRoot)
	require.Equal(t, "./cmd/server", docker.BuildTarget)
	require.False(t, docker.WithCGO)
}

func TestGoDockerTemplatingCollectsStandaloneRuntimeAssets(t *testing.T) {
	settings := &Settings{}
	settings.SourceDir = "code"
	settings.RuntimeAssets = []string{"routing", "config/prod.yaml"}

	_, assets, err := goDockerTemplating(settings, "/workspace", "/workspace/services/accounts")
	require.NoError(t, err)

	// Standalone service is itself the build context, so the context paths match
	// the declared, service-relative paths.
	require.Equal(t, []string{"routing", "config/prod.yaml"}, assets)
}

func TestGoDockerTemplatingPrefixesWorkspaceRuntimeAssets(t *testing.T) {
	workspace := t.TempDir()
	service := filepath.Join(workspace, "modules", "users", "services", "accounts")
	settings := &Settings{}
	settings.SourceDir = "code"
	settings.WithWorkspace = true
	settings.RuntimeAssets = []string{"routing"}

	_, assets, err := goDockerTemplating(settings, workspace, service)
	require.NoError(t, err)

	// Workspace build context is the workspace root, so the context path is
	// prefixed with the service-relative path. The template reproduces it at the
	// same path under /app, keeping it a sibling of the module dir.
	require.Equal(t, []string{"modules/users/services/accounts/routing"}, assets)
}

func TestGoDockerTemplatingRejectsInvalidRuntimeAsset(t *testing.T) {
	// Traversal/absolute paths escape the context; whitespace and glob
	// metacharacters would corrupt the generated `COPY <src> <dest>` (extra
	// tokens or source globbing), so all are rejected before templating.
	for _, asset := range []string{"../secrets", "/etc/passwd", ".", "", "my config", "tab\ther", "conf*", "a[b]"} {
		t.Run(asset, func(t *testing.T) {
			settings := &Settings{}
			settings.SourceDir = "code"
			settings.RuntimeAssets = []string{asset}

			_, _, err := goDockerTemplating(settings, "/workspace", "/workspace/services/accounts")
			require.Error(t, err)
		})
	}
}
