package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
	"github.com/codefly-dev/core/templates"
	"github.com/stretchr/testify/require"
)

func renderRuntimeStage(t *testing.T, packages []string) string {
	t.Helper()
	source, err := fs.ReadFile(builderFS, "templates/builder/Dockerfile.tmpl")
	require.NoError(t, err)
	rendered, err := templates.ApplyTemplate(string(source), dockerTemplating{
		DockerTemplating: golanghelpers.DockerTemplating{
			GoVersion:     GoVersion,
			AlpineVersion: AlpineVersion,
			SourceDir:     "code",
			ModuleRoot:    "code",
			BuildTarget:   ".",
		},
		RuntimePackages: packages,
	})
	require.NoError(t, err)
	start := strings.Index(rendered, "# Final stage")
	require.GreaterOrEqual(t, start, 0)
	return rendered[start:]
}

// TestDockerfileTemplateInstallsOnlyTheCABundleWhenNoRuntimePackagesDeclared
// holds the default: a service that declares nothing gets exactly the runtime
// stage it had before runtime-packages existed. The knob is additive, so an
// unset list — nil from an absent key, or an explicitly empty one — must not
// move a byte of the recipe, or every existing service's image changes on
// upgrade with no setting changed.
func TestDockerfileTemplateInstallsOnlyTheCABundleWhenNoRuntimePackagesDeclared(t *testing.T) {
	t.Parallel()

	const want = "# Runtime only needs the public CA bundle. Keep build tooling out of the image.\n" +
		"RUN apk add --no-cache ca-certificates\n"
	for _, packages := range [][]string{nil, {}} {
		stage := renderRuntimeStage(t, packages)
		require.Contains(t, stage, want)
		require.NotContains(t, stage, " git")
		require.Equal(t, 1, strings.Count(stage, "apk add"))
	}
	require.Equal(t, renderRuntimeStage(t, nil), renderRuntimeStage(t, []string{}))
}

// TestDockerfileTemplateInstallsDeclaredRuntimePackagesInTheFinalStage holds
// that a declared package reaches the image the service runs in. The builder
// stage already installs git for private module fetches, so a check that only
// searched the whole Dockerfile for "git" would pass while the runtime image
// still lacked it; the assertion is scoped to the final stage.
func TestDockerfileTemplateInstallsDeclaredRuntimePackagesInTheFinalStage(t *testing.T) {
	t.Parallel()

	stage := renderRuntimeStage(t, []string{"git", "openssh-client"})
	require.Contains(t, stage, "RUN apk add --no-cache ca-certificates git openssh-client\n")
	require.Contains(t, stage, "and the packages the service declares in runtime-packages")
	require.Equal(t, 1, strings.Count(stage, "apk add"))
}

// TestSettingsValidateRuntimePackages holds that every entry is a plain Alpine
// package name. The value is interpolated into a RUN line, so a pin, an option
// or a shell metacharacter would change what the build does rather than what it
// installs; each is refused with an error that names the key and the value.
func TestSettingsValidateRuntimePackages(t *testing.T) {
	tests := []struct {
		name     string
		packages []string
		wantErr  string
	}{
		{name: "unset", packages: nil},
		{name: "one package", packages: []string{"git"}},
		{name: "package name punctuation", packages: []string{"openssh-client", "libstdc++", "py3.12-foo", "font_x"}},
		{name: "version pin", packages: []string{"git=2.45.2-r0"}, wantErr: `"git=2.45.2-r0"`},
		{name: "version constraint", packages: []string{"git>2"}, wantErr: `"git>2"`},
		{name: "option", packages: []string{"--allow-untrusted"}, wantErr: `"--allow-untrusted"`},
		{name: "shell chaining", packages: []string{"git;rm"}, wantErr: `"git;rm"`},
		{name: "command substitution", packages: []string{"$(id)"}, wantErr: `"$(id)"`},
		{name: "whitespace", packages: []string{"git curl"}, wantErr: `"git curl"`},
		{name: "newline", packages: []string{"git\nRUN id"}, wantErr: `"git\nRUN id"`},
		{name: "uppercase", packages: []string{"Git"}, wantErr: `"Git"`},
		{name: "empty", packages: []string{""}, wantErr: `""`},
		{name: "too long", packages: []string{strings.Repeat("a", 65)}, wantErr: "runtime-packages"},
		{name: "duplicate", packages: []string{"git", "git"}, wantErr: `duplicate package "git"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Settings{RuntimePackages: tc.packages}).Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), "runtime-packages")
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestRuntimePackagesReachTheRecipeOverGRPC drives the public Build RPC so the
// setting is proven threaded from Settings to the emitted Dockerfile, not only
// renderable by the template in isolation.
func TestRuntimePackagesReachTheRecipeOverGRPC(t *testing.T) {
	identity := goGrpcServiceFixture(t)
	client, builder := startBuilderAgent(t, identity)
	builder.GoGrpc.Settings.RuntimePackages = []string{"git"}

	buildRecipePlan(context.Background(), t, client, identity)
	dockerfile, err := os.ReadFile(filepath.Join(identity.GetWorkspacePath(), identity.GetRelativeToWorkspace(), "builder", "Dockerfile"))
	require.NoError(t, err)
	stage := string(dockerfile)[strings.Index(string(dockerfile), "# Final stage"):]
	require.Contains(t, stage, "RUN apk add --no-cache ca-certificates git\n")
}

// TestRuntimePackagesBuildRefusalOverGRPC holds that the recipe render refuses a
// bad entry even when it reached the builder without passing Settings.Validate:
// no recipe is emitted, rather than one with the entry dropped or pasted.
func TestRuntimePackagesBuildRefusalOverGRPC(t *testing.T) {
	identity := goGrpcServiceFixture(t)
	client, builder := startBuilderAgent(t, identity)
	builder.GoGrpc.Settings.RuntimePackages = []string{"git; id"}
	request := &builderv0.BuildRequest{
		OutputDirectory: filepath.Join(identity.GetWorkspacePath(), identity.GetRelativeToWorkspace(), "builder"),
		BuildContext: &builderv0.BuildContext{Kind: &builderv0.BuildContext_DockerBuildContext{
			DockerBuildContext: &builderv0.DockerBuildContext{DockerRepository: "registry.example.com"},
		}},
	}
	response, err := client.Build(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_ERROR, response.GetState().GetState())
	require.Contains(t, response.GetState().GetMessage(), `runtime-packages: "git; id"`)
	require.Nil(t, response.GetResult().GetDockerBuildPlan())
	require.NoDirExists(t, request.OutputDirectory)
}
