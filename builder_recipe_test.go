package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
	"github.com/codefly-dev/core/templates"
	"github.com/stretchr/testify/require"
)

// renderBuilderTree renders the real builder templates into dir, mirroring what
// Build writes to the caller's output_directory before emitting a plan.
func renderBuilderTree(t *testing.T, dir string) {
	t.Helper()
	data := dockerTemplating{DockerTemplating: golanghelpers.DockerTemplating{
		GoVersion:     GoVersion,
		AlpineVersion: AlpineVersion,
		SourceDir:     "code",
		ModuleRoot:    "code",
		BuildTarget:   ".",
	}}
	for _, file := range []struct{ src, dst string }{
		{"templates/builder/Dockerfile.tmpl", "Dockerfile"},
		{"templates/builder/dockerignore.tmpl", "dockerignore"},
	} {
		source, err := fs.ReadFile(builderFS, file.src)
		require.NoError(t, err)
		rendered, err := templates.ApplyTemplate(string(source), data)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, file.dst), []byte(rendered), 0o644))
	}
}

// TestRecipeBuildPlanEmitsVerifiableSingleImageRecipe asserts the plan this
// agent emits from its rendered builder/ tree is one the CLI accepts: it passes
// VerifyDockerBuildPlan (the check the CLI runs before docker buildx) and names
// the same image, a service-directory context, and a multi-arch target.
func TestRecipeBuildPlanEmitsVerifiableSingleImageRecipe(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()
	renderBuilderTree(t, outputDir)

	image := &resources.DockerImage{Name: "mod/svc", Tag: "0.0.0"}

	plan, err := recipeBuildPlan(outputDir, image)
	require.NoError(t, err)

	// The CLI verifies the emitted tree against the plan before running buildx;
	// a plan this agent emits must survive that verification unchanged.
	require.NoError(t, services.VerifyDockerBuildPlan(outputDir, plan))

	require.Len(t, plan.GetRecipes(), 1)
	recipe := plan.GetRecipes()[0]
	require.Equal(t, "Dockerfile", recipe.GetDockerfile())
	require.Equal(t, ".", recipe.GetContext())
	require.Equal(t, "dockerignore", recipe.GetDockerignore())
	require.Equal(t, image.FullName(), recipe.GetImage())
	// The deployment architecture (amd64) must be present or the CLI refuses to
	// push, and arm64 makes the manifest list pullable on either architecture.
	require.Equal(t, []string{"linux/amd64", "linux/arm64"}, recipe.GetPlatforms())
}

// TestRecipeBuildPlanRejectsUnrenderedTree asserts the plan builder surfaces a
// contract violation rather than emitting a recipe that points buildx at a
// missing Dockerfile — the tree must be rendered before the plan is built.
func TestRecipeBuildPlanRejectsUnrenderedTree(t *testing.T) {
	t.Parallel()

	_, err := recipeBuildPlan(t.TempDir(), &resources.DockerImage{Name: "mod/svc", Tag: "0.0.0"})
	require.Error(t, err)
}

func TestBuildRecipeOverGRPC(t *testing.T) {
	ctx := context.Background()
	identity := goGrpcServiceFixture(t)
	tmpDir := identity.GetWorkspacePath()
	client, builder := startBuilderAgent(t, identity)
	t.Setenv("PATH", t.TempDir())

	// Core's client probes this before dispatching a build that names an explicit
	// Buildx builder, and refuses the build outright when the answer is false.
	capabilities, err := client.BuildCapabilities(ctx, &builderv0.BuildCapabilitiesRequest{})
	require.NoError(t, err)
	require.True(t, capabilities.GetBuildxSelection())

	outputDir := filepath.Join(tmpDir, "mod/svc", "builder")
	for _, directory := range []string{"", "relative"} {
		resp, err := client.Build(ctx, &builderv0.BuildRequest{OutputDirectory: directory})
		require.NoError(t, err)
		require.Equal(t, builderv0.BuildStatus_ERROR, resp.GetState().GetState())
		require.Contains(t, resp.GetState().GetMessage(), "output_directory")
		require.NoDirExists(t, outputDir)
	}
	// A request that carries no Docker build context has no image name for the
	// recipe. It must fail as an error before rendering anything, not as the
	// UNKNOWN state a caught nil dereference would produce.
	resp, err := client.Build(ctx, &builderv0.BuildRequest{OutputDirectory: outputDir})
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_ERROR, resp.GetState().GetState())
	require.Contains(t, resp.GetState().GetMessage(), "docker build context")
	require.NoDirExists(t, outputDir)

	// A workspace service is rejected even for an otherwise complete request: the
	// recipe contract cannot express a build context above the service directory,
	// and there is no agent executor left to fall back to.
	builder.GoGrpc.Settings.WithWorkspace = true
	resp, err = client.Build(ctx, &builderv0.BuildRequest{
		BuildContext: &builderv0.BuildContext{Kind: &builderv0.BuildContext_DockerBuildContext{
			DockerBuildContext: &builderv0.DockerBuildContext{DockerRepository: "registry.example.com"},
		}},
		OutputDirectory: outputDir,
	})
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_ERROR, resp.GetState().GetState())
	require.Contains(t, resp.GetState().GetMessage(), "workspace")
	require.NoDirExists(t, outputDir)
	builder.GoGrpc.Settings.WithWorkspace = false
	resp, err = client.Build(ctx, &builderv0.BuildRequest{
		BuildContext: &builderv0.BuildContext{Kind: &builderv0.BuildContext_DockerBuildContext{
			DockerBuildContext: &builderv0.DockerBuildContext{DockerRepository: "registry.example.com"},
		}},
		OutputDirectory: outputDir,
	})
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_SUCCESS, resp.GetState().GetState(), resp.GetState().GetMessage())

	plan := resp.GetResult().GetDockerBuildPlan()
	require.NotNil(t, plan, "Build with output_directory must emit a DockerBuildPlan, not an in-process image")
	// The tree must land in output_directory, the exact place the CLI verifies.
	require.FileExists(t, filepath.Join(outputDir, "Dockerfile"))
	require.NoError(t, services.VerifyDockerBuildPlan(outputDir, plan))

	require.Len(t, plan.GetRecipes(), 1)
	recipe := plan.GetRecipes()[0]
	require.Equal(t, ".", recipe.GetContext())
	require.Equal(t, "registry.example.com/mod/svc:0.0.0", recipe.GetImage())
	require.Equal(t, []string{"linux/amd64", "linux/arm64"}, recipe.GetPlatforms())

	// A rebuild must overwrite the previously emitted recipe. Corrupt the tree
	// and rebuild: a stale Dockerfile would leave the plan referencing content the
	// current render never produced, so the emitted tree must not keep the sentinel.
	require.NoError(t, os.WriteFile(filepath.Join(outputDir, "Dockerfile"), []byte("FROM stale:sentinel\n"), 0o644))
	resp, err = client.Build(ctx, &builderv0.BuildRequest{
		BuildContext: &builderv0.BuildContext{Kind: &builderv0.BuildContext_DockerBuildContext{
			DockerBuildContext: &builderv0.DockerBuildContext{DockerRepository: "registry.example.com"},
		}},
		OutputDirectory: outputDir,
	})
	require.NoError(t, err)
	require.NotNil(t, resp.GetResult().GetDockerBuildPlan())
	require.NoError(t, services.VerifyDockerBuildPlan(outputDir, resp.GetResult().GetDockerBuildPlan()))
	rebuilt, err := os.ReadFile(filepath.Join(outputDir, "Dockerfile"))
	require.NoError(t, err)
	require.NotContains(t, string(rebuilt), "stale:sentinel")
	for _, dockerContext := range []*builderv0.DockerBuildContext{
		{DockerRepository: "registry.example.com", BuildxBuilder: "explicit-builder"},
		{DockerRepository: "registry.example.com", BuildxBuilder: "cache-builder", Cache: &builderv0.BuildCacheOptions{Backend: "registry", Scope: "service", Imports: []string{"registry.example.com/cache"}, Exports: []string{"registry.example.com/cache"}}},
	} {
		resp, err := client.Build(ctx, &builderv0.BuildRequest{
			OutputDirectory: outputDir,
			BuildContext:    &builderv0.BuildContext{Kind: &builderv0.BuildContext_DockerBuildContext{DockerBuildContext: dockerContext}},
		})
		require.NoError(t, err)
		require.Equal(t, builderv0.BuildStatus_SUCCESS, resp.GetState().GetState(), resp.GetState().GetMessage())
		require.NoError(t, services.VerifyDockerBuildPlan(outputDir, resp.GetResult().GetDockerBuildPlan()))
		require.Equal(t, plan.GetDigest(), resp.GetResult().GetDockerBuildPlan().GetDigest())
		require.Nil(t, resp.GetResult().GetDockerBuildResult())
		require.Empty(t, resp.GetBuildxBuilder())
		require.Empty(t, resp.GetCacheContractVersion())
	}
}
