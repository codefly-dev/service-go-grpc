package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
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

// TestBuildEmitsRecipePlanWhenCallerOwnsBuild drives Build end to end (short of
// Docker) for the CLI-owned-build path: a non-workspace service asked to emit
// into output_directory must render its builder/ tree there and return a
// DockerBuildPlan the CLI accepts, not run an in-process build.
func TestBuildEmitsRecipePlanWhenCallerOwnsBuild(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	service := &resources.Service{Name: "svc", Version: "0.0.0"}
	require.NoError(t, service.SaveAtDir(ctx, filepath.Join(tmpDir, "mod/svc")))
	service.WithModule("mod")
	require.NoError(t, (&resources.Module{Name: "mod"}).SaveToDir(ctx, filepath.Join(tmpDir, "mod")))

	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Version:             service.Version,
		Module:              "mod",
		Workspace:           "test",
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: "mod/svc",
	}

	builder := NewBuilder(NewService())
	// CreationMode short-circuits endpoint discovery, which the recipe path does
	// not need — it renders templates and inventories the tree, never compiling.
	_, err := builder.Load(ctx, &builderv0.LoadRequest{Identity: identity, CreationMode: &builderv0.CreationMode{}})
	require.NoError(t, err)

	outputDir := filepath.Join(tmpDir, "mod/svc", "builder")
	resp, err := builder.Build(ctx, &builderv0.BuildRequest{
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
	resp, err = builder.Build(ctx, &builderv0.BuildRequest{
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
}

func TestShouldEmitBuildRecipe(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		outputDir   string
		contextRoot string
		want        bool
	}{
		{"caller owns build, service-directory context", "/out", "", true},
		{"no output directory keeps the in-process build", "", "", false},
		{"workspace context cannot be expressed as a recipe", "/out", "/workspace", false},
		{"no output directory, workspace context", "", "/workspace", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, shouldEmitBuildRecipe(tc.outputDir, tc.contextRoot))
		})
	}
}
