package main

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/resources"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
	"github.com/stretchr/testify/require"
)

// TestServiceRecipeDeclaresItsGoModuleDownload pins the declaration the CLI
// fetches from: a service built from its own directory declares its module root
// (relative to that directory, the recipe's context) and the gomodproxy
// context, so the plan carries the v5 contract and still verifies.
func TestServiceRecipeDeclaresItsGoModuleDownload(t *testing.T) {
	t.Parallel()
	outputDir := t.TempDir()
	renderBuilderTree(t, outputDir)

	options := goModuleDownloads(golanghelpers.DockerTemplating{ModuleRoot: "code"})
	plan, err := recipeBuildPlan(outputDir, &resources.DockerImage{Name: "mod/svc", Tag: "0.0.0"}, []string{"Dockerfile", "dockerignore"}, options...)
	require.NoError(t, err)
	require.NoError(t, services.VerifyDockerBuildPlan(outputDir, plan))
	require.Equal(t, services.DockerBuildRecipeGoModulesContractVersion, plan.GetContractVersion())
	downloads := plan.GetRecipes()[0].GetGoModuleDownloads()
	require.Len(t, downloads, 1)
	require.Equal(t, "code", downloads[0].GetModuleRoot())
	require.Equal(t, goModuleProxyContext, downloads[0].GetProxyContext())
}

// TestOnlyAServiceRootedBuildDeclaresDownloads keeps the declaration honest: a
// workspace build's context is not the service directory, and a build without a
// module root decides where go.mod is at build time, so neither names a root the
// caller could fetch.
func TestOnlyAServiceRootedBuildDeclaresDownloads(t *testing.T) {
	t.Parallel()
	require.Empty(t, goModuleDownloads(golanghelpers.DockerTemplating{ModuleRoot: "services/api/code", Workspace: true}))
	require.Empty(t, goModuleDownloads(golanghelpers.DockerTemplating{}))
}

// TestTemplateReadsTheDeclaredProxyWithoutACredential pins the template side of
// the contract: a stage named after the declared context, read as the only
// GOPROXY when supplied, and no credential mount on that download.
func TestTemplateReadsTheDeclaredProxyWithoutACredential(t *testing.T) {
	t.Parallel()
	template, err := fs.ReadFile(builderFS, "templates/builder/Dockerfile.tmpl")
	require.NoError(t, err)
	source := string(template)

	require.Contains(t, source, "FROM scratch AS "+goModuleProxyContext)
	require.Less(t, strings.Index(source, "FROM scratch AS "+goModuleProxyContext), strings.Index(source, "AS builder"))

	start := strings.Index(source, "COPY {{ .ModuleRoot }}/go.mod {{ .ModuleRoot }}/go.sum {{ .ModuleRoot }}/")
	require.Positive(t, start)
	download := source[start:]
	download = download[:strings.Index(download, "COPY {{ .ModuleRoot }} {{ .ModuleRoot }}")]
	require.Contains(t, download, "--mount=type=bind,from="+goModuleProxyContext+",target=/gomodproxy")
	require.Contains(t, download, "GOPROXY=file:///gomodproxy")
	require.Contains(t, download, "GOPRIVATE=")
	require.NotContains(t, download, "type=secret", "the declared download needs no credential")
}
