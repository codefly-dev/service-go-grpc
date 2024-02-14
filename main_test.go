package main_test

import (
	"context"
	"os"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"

	"github.com/codefly-dev/core/configurations"

	"github.com/stretchr/testify/assert"

	basev0 "github.com/codefly-dev/core/generated/go/base/v0"

	main "github.com/codefly-dev/service-go-grpc"
)

func TestCreate(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	defer func(path string) {
		err := os.RemoveAll(path)
		if err != nil {
			t.Fatal(err)
		}
	}(tmpDir)

	conf := configurations.Service{Name: "svc", Application: "app"}
	err := conf.SaveAtDir(ctx, tmpDir)
	assert.NoError(t, err)
	identity := &basev0.ServiceIdentity{
		Name:        "svc",
		Application: "app",
		Location:    tmpDir,
	}
	builder := main.NewBuilder()
	resp, err := builder.Load(ctx, &builderv0.LoadRequest{Identity: identity})
	assert.NoError(t, err)
	assert.NotNil(t, resp)

}
