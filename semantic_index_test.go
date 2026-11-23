package main

import (
	"os"
	"path/filepath"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	gocode "github.com/codefly-dev/service-go/pkg/code"
	"github.com/stretchr/testify/require"
)

// Usage lint needs calls from package initializers on the same Code surface
// it queries for calls in functions.
func TestCodeIndexesCallsInPackageInitializers(t *testing.T) {
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "main.go"), []byte("package main\n\nimport \"log/slog\"\n\nvar logger = slog.Default()\n"), 0o600))
	service := NewService()
	service.SourceLocation = source
	code := gocode.New(service.Service)

	response, err := code.Execute(t.Context(), &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_GetSemanticIndex{GetSemanticIndex: &codev0.GetSemanticIndexRequest{}},
	})
	require.NoError(t, err)
	require.Nil(t, response.GetFailure())
	index := response.GetGetSemanticIndex()
	require.NotNil(t, index)
	require.Equal(t, basev0.SemanticIndexState_SEMANTIC_INDEX_STATE_COMPLETE, index.GetState())
	require.True(t, index.GetFileCallsProjected())
	require.Len(t, index.GetFiles(), 1)
	file := index.GetFiles()[0]
	require.Equal(t, "main.go", file.GetPath())
	require.Len(t, file.GetCalls(), 1)
	require.Equal(t, "slog.Default", file.GetCalls()[0].GetName())
	require.EqualValues(t, 5, file.GetCalls()[0].GetLocation().GetStartLine())
}
