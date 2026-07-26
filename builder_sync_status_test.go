package main

import (
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

func TestSyncResponseError(t *testing.T) {
	tests := []struct {
		name     string
		response *builderv0.SyncResponse
		want     string
	}{
		{name: "missing response", want: "sync returned no status"},
		{name: "missing status", response: &builderv0.SyncResponse{}, want: "sync returned no status"},
		{
			name: "encoded failure",
			response: &builderv0.SyncResponse{State: &builderv0.SyncStatus{
				State:   builderv0.SyncStatus_ERROR,
				Message: "buf generation failed",
			}},
			want: "buf generation failed",
		},
		{
			name: "status fallback",
			response: &builderv0.SyncResponse{State: &builderv0.SyncStatus{
				State: builderv0.SyncStatus_UNSUPPORTED,
			}},
			want: "UNSUPPORTED",
		},
		{
			name: "success",
			response: &builderv0.SyncResponse{State: &builderv0.SyncStatus{
				State: builderv0.SyncStatus_SUCCESS,
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := syncResponseError(test.response)
			if test.want == "" {
				if err != nil {
					t.Fatalf("syncResponseError() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != test.want {
				t.Fatalf("syncResponseError() = %v, want %q", err, test.want)
			}
		})
	}
}
