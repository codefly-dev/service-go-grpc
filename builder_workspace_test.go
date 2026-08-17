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
	configure, err := goDockerTemplating(settings, workspace, symlinkService)
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

	configure, err := goDockerTemplating(settings, workspace, service)
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

	configure, err := goDockerTemplating(settings, "/workspace", "/workspace/modules/users/services/accounts")
	require.NoError(t, err)
	var docker golanghelpers.DockerTemplating
	configure(&docker)

	require.False(t, docker.Workspace)
	require.Empty(t, docker.ContextRoot)
	require.Equal(t, "code", docker.ModuleRoot)
	require.Equal(t, "./cmd/server", docker.BuildTarget)
	require.False(t, docker.WithCGO)
}
