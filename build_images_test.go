package main

import (
	"io/fs"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/templates"
	"github.com/stretchr/testify/require"
)

// A cell selection must reach the real template while retaining the native vs
// scratch decision. Changing the selector alone cannot implement another ABI.
func TestEnvironmentImageBindingsReachRealRecipe(t *testing.T) {
	source, err := fs.ReadFile(builderFS, "templates/builder/Dockerfile.tmpl")
	require.NoError(t, err)
	for _, native := range []bool{false, true} {
		data := dockerTemplating{}
		data.WithCGO = native
		role := "runtime-support"
		if native {
			role = "runtime"
		}
		replacement := "registry.example/approved/alpine@sha256:" + strings.Repeat("a", 64)
		binding := &builderv0.BuildImageInput{Role: role, Reference: replacement, Variant: "alpine3.23-musl"}
		require.NoError(t, resolveTemplateImages(&data, []*builderv0.BuildImageInput{binding}))
		rendered, err := templates.ApplyTemplate(string(source), data)
		require.NoError(t, err)
		require.Contains(t, rendered, "FROM "+replacement+" AS runtimefiles")
		require.NotContains(t, rendered, "FROM docker.io/library/alpine:")
		final := rendered[strings.LastIndex(rendered, "\nFROM ")+1:]
		if native {
			require.Contains(t, final, "FROM "+replacement)
		} else {
			require.True(t, strings.HasPrefix(final, "FROM scratch\n"))
		}
		binding.Variant = "wolfi-glibc"
		require.ErrorContains(t, resolveTemplateImages(&data, []*builderv0.BuildImageInput{binding}), "unsupported recipe variant")
	}
}
