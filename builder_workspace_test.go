package main

import (
	"path/filepath"
	"testing"

	golanghelpers "github.com/codefly-dev/core/runners/golang"
	"github.com/stretchr/testify/require"
)

func TestGoDockerTemplatingUsesWorkspaceForLocalModuleReplacements(t *testing.T) {
	workspace := t.TempDir()
	service := filepath.Join(workspace, "modules", "users", "services", "forge-edge")
	settings := &Settings{}
	settings.SourceDir = "code/cmd/server"
	settings.WithWorkspace = true

	configure, err := goDockerTemplating(settings, workspace, service)
	require.NoError(t, err)
	var docker golanghelpers.DockerTemplating
	configure(&docker)

	require.True(t, docker.Workspace)
	require.Equal(t, workspace, docker.ContextRoot)
	require.Equal(t, "modules/users/services/forge-edge/code", docker.ModuleRoot)
	require.Equal(t, "./cmd/server", docker.BuildTarget)
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
}
