package main

import (
	"io/fs"
	"strings"
	"testing"

	golanghelpers "github.com/codefly-dev/core/runners/golang"
	"github.com/codefly-dev/core/templates"
	"github.com/stretchr/testify/require"
)

func TestDockerfileTemplateUsesPinnedMinimalImages(t *testing.T) {
	t.Parallel()

	template, err := fs.ReadFile(builderFS, "templates/builder/Dockerfile.tmpl")
	if err != nil {
		t.Fatalf("read Dockerfile template: %v", err)
	}
	source := string(template)

	if !strings.Contains(source, "FROM golang:{{ .GoVersion }}-alpine3.23@sha256:") {
		t.Fatal("builder image must use the exact Go version and an immutable digest")
	}
	if !strings.Contains(source, "FROM alpine:{{ .AlpineVersion }}@sha256:") {
		t.Fatal("runtime image must use the exact Alpine version and an immutable digest")
	}

	runtimeStart := strings.Index(source, "# Final stage")
	if runtimeStart < 0 {
		t.Fatal("runtime stage marker is missing")
	}
	runtimeStage := source[runtimeStart:]
	if strings.Contains(runtimeStage, "ca-certificates git") {
		t.Fatal("runtime image must not include git")
	}
	if !strings.Contains(runtimeStage, "RUN apk add --no-cache ca-certificates") {
		t.Fatal("runtime image must retain the CA bundle")
	}
}

func TestDockerfileTemplateCarriesModuleFixturesForWorkspaceBuilds(t *testing.T) {
	t.Parallel()

	template, err := fs.ReadFile(builderFS, "templates/builder/Dockerfile.tmpl")
	if err != nil {
		t.Fatalf("read Dockerfile template: %v", err)
	}
	source := string(template)

	if !strings.Contains(source, `fixture_root="$(dirname "$(dirname "$(dirname "{{ .ModuleRoot }}")")")/fixtures"`) {
		t.Fatal("workspace build must locate the containing module fixtures")
	}
	if !strings.Contains(source, "if [ -d \"$fixture_root\" ]; then cp -R \"$fixture_root\"/. /app/runtime-fixtures/; fi") {
		t.Fatal("workspace build must stage only module fixture files when present")
	}
	if !strings.Contains(source, "COPY --chown=appuser --from=builder /app/runtime-fixtures ./fixtures") {
		t.Fatal("workspace runtime must include the staged module fixtures")
	}
}

func TestDockerfileTemplatePackagesContainingModuleFixturesForWorkspaceBuilds(t *testing.T) {
	t.Parallel()

	source, err := fs.ReadFile(builderFS, "templates/builder/Dockerfile.tmpl")
	require.NoError(t, err)
	rendered, err := templates.ApplyTemplate(string(source), dockerTemplating{DockerTemplating: golanghelpers.DockerTemplating{
		GoVersion:     GoVersion,
		AlpineVersion: AlpineVersion,
		ModuleRoot:    "modules/users/services/accounts/code",
		BuildTarget:   ".",
		Workspace:     true,
	}})
	require.NoError(t, err)

	require.Contains(t, rendered, `-o /app/app .`)
	require.Contains(t, rendered, `fixture_root="$(dirname "$(dirname "$(dirname "modules/users/services/accounts/code")")")/fixtures"`)
	require.Contains(t, rendered, `mkdir -p /app/runtime-fixtures`)
	require.Contains(t, rendered, `if [ -d "$fixture_root" ]; then cp -R "$fixture_root"/. /app/runtime-fixtures/; fi`)
	require.Contains(t, rendered, `COPY --chown=appuser --from=builder /app/app .`)
	require.Contains(t, rendered, `COPY --chown=appuser --from=builder /app/runtime-fixtures ./fixtures`)
}

func TestDockerfileTemplateCopiesRuntimeAssetsIntoFinalStage(t *testing.T) {
	t.Parallel()

	source, err := fs.ReadFile(builderFS, "templates/builder/Dockerfile.tmpl")
	require.NoError(t, err)
	rendered, err := templates.ApplyTemplate(string(source), dockerTemplating{
		DockerTemplating: golanghelpers.DockerTemplating{
			GoVersion:     GoVersion,
			AlpineVersion: AlpineVersion,
			ModuleRoot:    "code",
			BuildTarget:   ".",
		},
		RuntimeAssets: []dockerRuntimeAsset{
			{Source: "routing", Dest: "routing"},
			{Source: "config/prod.yaml", Dest: "config/prod.yaml"},
		},
	})
	require.NoError(t, err)

	runtimeStage := rendered[strings.Index(rendered, "# Final stage"):]
	require.Contains(t, runtimeStage, "COPY --chown=appuser routing routing")
	require.Contains(t, runtimeStage, "COPY --chown=appuser config/prod.yaml config/prod.yaml")
}

func TestDockerfileTemplateOmitsRuntimeAssetCopiesWhenNoneDeclared(t *testing.T) {
	t.Parallel()

	source, err := fs.ReadFile(builderFS, "templates/builder/Dockerfile.tmpl")
	require.NoError(t, err)
	rendered, err := templates.ApplyTemplate(string(source), dockerTemplating{DockerTemplating: golanghelpers.DockerTemplating{
		GoVersion:     GoVersion,
		AlpineVersion: AlpineVersion,
		ModuleRoot:    "code",
		BuildTarget:   ".",
	}})
	require.NoError(t, err)

	require.NotContains(t, rendered[strings.Index(rendered, "# Final stage"):], "COPY --chown=appuser routing")
}

func TestDockerfileTemplateLeavesStandaloneRuntimeUnchanged(t *testing.T) {
	t.Parallel()

	source, err := fs.ReadFile(builderFS, "templates/builder/Dockerfile.tmpl")
	require.NoError(t, err)
	rendered, err := templates.ApplyTemplate(string(source), dockerTemplating{DockerTemplating: golanghelpers.DockerTemplating{
		GoVersion:     GoVersion,
		AlpineVersion: AlpineVersion,
		ModuleRoot:    "code",
		BuildTarget:   ".",
	}})
	require.NoError(t, err)

	require.Contains(t, rendered, `-o /app/app .`)
	require.Contains(t, rendered, `COPY --chown=appuser --from=builder /app/app .`)
	require.NotContains(t, rendered, `fixture_root=`)
	require.NotContains(t, rendered, `runtime-fixtures`)
}

func TestDockerfileTemplateHonorsCGOServiceContract(t *testing.T) {
	t.Parallel()

	source, err := fs.ReadFile(builderFS, "templates/builder/Dockerfile.tmpl")
	require.NoError(t, err)
	rendered, err := templates.ApplyTemplate(string(source), dockerTemplating{DockerTemplating: golanghelpers.DockerTemplating{
		GoVersion: GoVersion, AlpineVersion: AlpineVersion,
		ModuleRoot: "code", BuildTarget: "./cmd/server", WithCGO: true,
	}})
	require.NoError(t, err)

	require.Contains(t, rendered, `apk add --no-cache ca-certificates git build-base`)
	require.Contains(t, rendered, `ENV CGO_ENABLED=1`)
	require.Contains(t, rendered, `go build -ldflags='-w -s' -o /app/app ./cmd/server`)
	require.NotContains(t, rendered, `extldflags "-static"`)
}

func TestDockerfileTemplateKeepsNonCGOBuildStatic(t *testing.T) {
	t.Parallel()

	source, err := fs.ReadFile(builderFS, "templates/builder/Dockerfile.tmpl")
	require.NoError(t, err)
	rendered, err := templates.ApplyTemplate(string(source), dockerTemplating{DockerTemplating: golanghelpers.DockerTemplating{
		GoVersion: GoVersion, AlpineVersion: AlpineVersion,
		ModuleRoot: "code", BuildTarget: "./cmd/server",
	}})
	require.NoError(t, err)

	require.Contains(t, rendered, `apk add --no-cache ca-certificates git`)
	require.NotContains(t, rendered, `git build-base`)
	require.Contains(t, rendered, `ENV CGO_ENABLED=0`)
	require.Contains(t, rendered, `extldflags "-static"`)
}
