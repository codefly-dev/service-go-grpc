package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/agents/services/sbom"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fixtureGoVersion is the go directive written into fixture modules. It is
// deliberately not this agent's GoVersion: the fixtures assert nothing about the
// toolchain, and naming a patch release newer than the one running the tests
// makes `go list` download a toolchain before it will answer.
const fixtureGoVersion = "1.21"

// goGrpcServiceFixture writes the on-disk service a Builder.Load accepts, plus a
// self-contained Go module at the source location so the source inventory
// resolves without reaching the network.
func goGrpcServiceFixture(t *testing.T) *basev0.ServiceIdentity {
	t.Helper()

	ctx := context.Background()
	workspacePath := t.TempDir()
	serviceDir := filepath.Join(workspacePath, "mod/svc")

	service := &resources.Service{Name: "svc", Version: "0.0.0"}
	require.NoError(t, service.SaveAtDir(ctx, serviceDir))
	service.WithModule("mod")
	require.NoError(t, (&resources.Module{Name: "mod"}).SaveToDir(ctx, filepath.Join(workspacePath, "mod")))

	codeDir := filepath.Join(serviceDir, "code")
	require.NoError(t, os.MkdirAll(codeDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(codeDir, "go.mod"), []byte("module example.com/svc\n\ngo "+fixtureGoVersion+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(codeDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644))

	return &basev0.ServiceIdentity{
		Name:                service.Name,
		Version:             service.Version,
		Module:              "mod",
		Workspace:           "test",
		WorkspacePath:       workspacePath,
		RelativeToWorkspace: "mod/svc",
	}
}

// startBuilderAgent serves this specialization's Builder over gRPC and returns a
// client plus the Builder itself, for callers that need agent state the wire
// does not expose. Build is go-grpc's own and SBOM is inherited from the generic
// Go builder, so only the wire surface proves the composed agent answers both.
func startBuilderAgent(t *testing.T, identity *basev0.ServiceIdentity) (*services.BuilderAgent, *Builder) {
	t.Helper()

	builder := NewBuilder(NewService())
	// CreationMode short-circuits endpoint discovery, which neither the recipe
	// nor the inventory path needs — they render templates and read the module
	// graph, never compiling.
	_, err := builder.Load(context.Background(), &builderv0.LoadRequest{Identity: identity, CreationMode: &builderv0.CreationMode{}})
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	builderv0.RegisterBuilderServer(server, builder)

	// Serve's result is handed back through a channel and reported during
	// cleanup rather than logged from the goroutine: a t.Errorf that lands after
	// the test function returns panics the whole package instead of failing one
	// test. Cleanups run last-registered-first, so the client below is closed
	// before the server is stopped.
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		if err := <-served; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("serve builder: %v", err)
		}
	})

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	return services.NewBuilderAgentClient(conn), builder
}

// buildRecipePlan drives this specialization's own Build and returns the plan it
// emits. The image subjects a caller must cover are derived from this plan, so
// coverage is measured against what go-grpc actually ships rather than a
// restatement of it.
func buildRecipePlan(ctx context.Context, t *testing.T, client *services.BuilderAgent, identity *basev0.ServiceIdentity) *builderv0.DockerBuildPlan {
	t.Helper()

	resp, err := client.Build(ctx, &builderv0.BuildRequest{
		OutputDirectory: filepath.Join(identity.GetWorkspacePath(), identity.GetRelativeToWorkspace(), "builder"),
		BuildContext: &builderv0.BuildContext{Kind: &builderv0.BuildContext_DockerBuildContext{
			DockerBuildContext: &builderv0.DockerBuildContext{DockerRepository: "registry.example.com"},
		}},
	})
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_SUCCESS, resp.GetState().GetState(), resp.GetState().GetMessage())

	plan := resp.GetResult().GetDockerBuildPlan()
	require.NotNil(t, plan)
	return plan
}

// TestSourceInventoryRemainsAvailable guards the half of the contract that was
// already here: image coverage is separate evidence, and adopting it must not
// cost the Go source inventory. The document's root component is asserted
// because a fixture module with no dependencies yields an empty component list,
// so a checksum alone would also be produced by an inventory that resolved
// nothing at all.
func TestSourceInventoryRemainsAvailable(t *testing.T) {
	ctx := context.Background()
	identity := goGrpcServiceFixture(t)
	client, _ := startBuilderAgent(t, identity)

	resp, err := client.SBOM(ctx, &builderv0.SBOMRequest{})
	require.NoError(t, err)
	require.Equal(t, builderv0.SBOMStatus_COMPLETE, resp.GetState().GetState(), resp.GetState().GetMessage())
	require.Equal(t, builderv0.SBOMScope_SBOM_SCOPE_SOURCE, resp.GetScope())
	require.NotEmpty(t, resp.GetSha256())
	require.Equal(t, "pkg:golang/example.com/svc", resp.GetBom().GetMetadata().GetComponent().GetPurl())
}

// TestImageScopeWithoutSubjectsIsAPreconditionFailure proves the inherited
// image-scope path is reachable through the composed agent. This specialization
// overrides Load and Build and embeds the generic builder for the rest, so only
// the wire surface distinguishes an image-scope request that reaches the shared
// implementation from one that silently falls back to a source inventory — which
// is exactly what this agent did before the contract was adopted.
func TestImageScopeWithoutSubjectsIsAPreconditionFailure(t *testing.T) {
	ctx := context.Background()
	identity := goGrpcServiceFixture(t)
	client, _ := startBuilderAgent(t, identity)

	resp, err := client.SBOM(ctx, &builderv0.SBOMRequest{Scope: builderv0.SBOMScope_SBOM_SCOPE_IMAGE})
	require.NoError(t, err)
	require.Equal(t, builderv0.SBOMStatus_ERROR, resp.GetState().GetState())
	require.Equal(t, builderv0.SBOMScope_SBOM_SCOPE_IMAGE, resp.GetScope())
	require.Equal(t, basev0.FailureCode_FAILURE_CODE_PRECONDITION_FAILED, resp.GetState().GetFailure().GetCode())
}

// TestPlanDerivedSubjectsAreRefusedUntilTheyCarryADigest closes the loop between
// this specialization's recipe and the evidence contract. The recipe names the
// image the caller has yet to build, so subjects derived from the plan carry a
// tag and no digest. Handing them straight back must not yield coverage for
// whatever that tag resolves to when the scan runs.
//
// It also pins the shape of that subject set: one per shipped platform, since
// evidence for one architecture of a multi-architecture image is not coverage of
// the other, and a recipe narrowed to a single platform would silently shrink
// what a caller is required to produce.
func TestPlanDerivedSubjectsAreRefusedUntilTheyCarryADigest(t *testing.T) {
	ctx := context.Background()
	identity := goGrpcServiceFixture(t)
	client, _ := startBuilderAgent(t, identity)

	plan := buildRecipePlan(ctx, t, client, identity)
	require.Len(t, plan.GetRecipes(), 1)
	recipe := plan.GetRecipes()[0]
	require.NotEmpty(t, recipe.GetPlatforms())

	unique := resources.ServiceUnique(identity.GetModule(), identity.GetName())
	expected := sbom.ExpectedFromBuildPlan(unique, plan)

	require.Len(t, expected, len(recipe.GetPlatforms()))
	for i, platform := range recipe.GetPlatforms() {
		require.Equal(t, platform, expected[i].GetPlatform())
		require.Equal(t, recipe.GetName(), expected[i].GetRole())
		require.Equal(t, recipe.GetImage(), expected[i].GetReference())
		require.Equal(t, unique, expected[i].GetService())
		require.Empty(t, expected[i].GetDigest())
	}

	resp, err := client.SBOM(ctx, &builderv0.SBOMRequest{
		Scope:    builderv0.SBOMScope_SBOM_SCOPE_IMAGE,
		Subjects: expected,
	})
	require.NoError(t, err)
	require.Equal(t, builderv0.SBOMStatus_ERROR, resp.GetState().GetState())
	require.Equal(t, builderv0.SBOMScope_SBOM_SCOPE_IMAGE, resp.GetScope())
	require.Error(t, sbom.ValidateCoverage(expected, resp))
}
