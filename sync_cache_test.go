package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// A dry-run discards its staged output. Caching only generation inputs then
// accepts the still-corrupt checkout on the next invocation without generating.
func TestSyncNeverAcceptsDiscardedOrTamperedGeneratedOutput(t *testing.T) {
	protoCompanionMu.Lock()
	defer protoCompanionMu.Unlock()
	ctx := t.Context()
	root := t.TempDir()
	serviceDir := filepath.Join(root, "mod", "svc")
	service := &resources.Service{Name: "svc", Version: "0.0.0"}
	require.NoError(t, service.SaveAtDir(ctx, serviceDir))
	require.NoError(t, (&resources.Module{Name: "mod"}).SaveToDir(ctx, filepath.Join(root, "mod")))
	builder := NewBuilder(NewService())
	_, err := builder.Load(ctx, &builderv0.LoadRequest{
		Identity: &basev0.ServiceIdentity{
			Name: "svc", Version: "0.0.0", Module: "mod", Workspace: "test",
			WorkspacePath: root, RelativeToWorkspace: "mod/svc",
		},
		CreationMode: &builderv0.CreationMode{},
	})
	require.NoError(t, err)
	created, err := builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)
	require.Equal(t, builderv0.CreateStatus_CREATED, created.GetState().GetState(), created.GetState().GetMessage())
	var generated string
	require.NoError(t, filepath.WalkDir(filepath.Join(serviceDir, "code", "pkg", "gen"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if generated == "" && !entry.IsDir() && strings.HasSuffix(path, ".pb.go") {
			generated = path
		}
		return nil
	}))
	require.NotEmpty(t, generated)
	original, err := os.ReadFile(generated)
	require.NoError(t, err)
	corrupted := append(append([]byte{}, original...), []byte("\n// unexpected generated drift\n")...)
	require.NoError(t, os.WriteFile(generated, corrupted, 0o644))
	relative, err := filepath.Rel(root, generated)
	require.NoError(t, err)
	for range 2 {
		response, err := builder.Sync(ctx, &builderv0.SyncRequest{DryRun: true})
		require.NoError(t, err)
		require.Equal(t, builderv0.SyncStatus_SUCCESS, response.GetState().GetState(), response.GetState().GetMessage())
		require.Contains(t, response.GetChangedFiles(), filepath.ToSlash(relative))
		unchanged, err := os.ReadFile(generated)
		require.NoError(t, err)
		require.Equal(t, corrupted, unchanged, "dry-run must not repair the checkout")
	}
	response, err := builder.Sync(ctx, &builderv0.SyncRequest{})
	require.NoError(t, err)
	require.Equal(t, builderv0.SyncStatus_SUCCESS, response.GetState().GetState(), response.GetState().GetMessage())
	repaired, err := os.ReadFile(generated)
	require.NoError(t, err)
	require.Equal(t, original, repaired)
	response, err = builder.Sync(ctx, &builderv0.SyncRequest{DryRun: true})
	require.NoError(t, err)
	require.Equal(t, builderv0.SyncStatus_SUCCESS, response.GetState().GetState(), response.GetState().GetMessage())
	require.Empty(t, response.GetChangedFiles())
	require.NoError(t, os.Remove(generated))
	response, err = builder.Sync(ctx, &builderv0.SyncRequest{DryRun: true})
	require.NoError(t, err)
	require.Equal(t, builderv0.SyncStatus_SUCCESS, response.GetState().GetState(), response.GetState().GetMessage())
	require.Contains(t, response.GetChangedFiles(), filepath.ToSlash(relative), "missing output must invalidate any prior generation success")
	require.NoFileExists(t, generated)
}
