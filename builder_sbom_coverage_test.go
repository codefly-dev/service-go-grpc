package main

import (
	"context"
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
	require.NoError(t, os.WriteFile(filepath.Join(codeDir, "go.mod"), []byte("module example.com/svc\n\ngo "+GoVersion+"\n"), 0o644))
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

// startBuilderAgent serves this specialization's Builder over gRPC. Build is
// go-grpc's own and SBOM is inherited from the generic Go builder, so only the
// wire surface proves the composed agent answers both.
func startBuilderAgent(t *testing.T, identity *basev0.ServiceIdentity) *services.BuilderAgent {
	t.Helper()

	builder := NewBuilder(NewService())
	_, err := builder.Load(context.Background(), &builderv0.LoadRequest{Identity: identity, CreationMode: &builderv0.CreationMode{}})
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	builderv0.RegisterBuilderServer(server, builder)
	go func() {
		if err := server.Serve(listener); err != nil {
			t.Errorf("serve builder: %v", err)
		}
	}()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	return services.NewBuilderAgentClient(conn)
}

// buildRecipePlan drives this specialization's own Build and returns the plan it
// emits. The image subjects a caller must cover are derived from this plan, so
// coverage is measured against what go-grpc actually ships rather than a
// restatement of it.
func buildRecipePlan(t *testing.T, ctx context.Context, client *services.BuilderAgent, identity *basev0.ServiceIdentity) *builderv0.DockerBuildPlan {
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

// TestImageSubjectsCoverEveryPlatformGoGrpcShips asserts the coverage
// expectation derived from this specialization's own recipe: one subject per
// shipped platform, carrying the recipe's role and the service identity. A
// recipe narrowed to a single platform, or one that stopped naming its role,
// would silently shrink what image evidence is required.
func TestImageSubjectsCoverEveryPlatformGoGrpcShips(t *testing.T) {
	ctx := context.Background()
	identity := goGrpcServiceFixture(t)
	client := startBuilderAgent(t, identity)

	plan := buildRecipePlan(t, ctx, client, identity)
	require.Len(t, plan.GetRecipes(), 1)
	recipe := plan.GetRecipes()[0]
	platforms := recipe.GetPlatforms()
	require.NotEmpty(t, platforms)

	unique := resources.ServiceUnique(identity.GetModule(), identity.GetName())
	expected := sbom.ExpectedFromBuildPlan(unique, plan)

	require.Len(t, expected, len(platforms))
	for i, platform := range platforms {
		require.Equal(t, platform, expected[i].GetPlatform())
		require.Equal(t, recipe.GetName(), expected[i].GetRole())
		require.Equal(t, recipe.GetImage(), expected[i].GetReference())
		require.Equal(t, unique, expected[i].GetService())
	}
}

// TestImageCoverageIsNeverClaimedWithoutImageEvidence is this specialization's
// release gate. The agent emits a recipe and never builds an image, so it holds
// no digest of its own: whatever it answers an image-scope request with, that
// answer must not pass as coverage of the images its recipe produces.
//
// The assertion is deliberately only that coverage validation fails. Which
// failure is correct belongs to the shared contract — a source inventory
// answering an image request, or the precondition failure an agent whose caller
// owns the build returns — and pinning either message here would make this test
// a mirror of the shared implementation rather than a gate on this agent.
func TestImageCoverageIsNeverClaimedWithoutImageEvidence(t *testing.T) {
	ctx := context.Background()
	identity := goGrpcServiceFixture(t)
	client := startBuilderAgent(t, identity)

	plan := buildRecipePlan(t, ctx, client, identity)
	expected := sbom.ExpectedFromBuildPlan(resources.ServiceUnique(identity.GetModule(), identity.GetName()), plan)
	require.NotEmpty(t, expected)

	resp, err := client.SBOM(ctx, &builderv0.SBOMRequest{Scope: builderv0.SBOMScope_SBOM_SCOPE_IMAGE})
	require.NoError(t, err)
	require.Error(t, sbom.ValidateCoverage(expected, resp),
		"an image-scope request answered without digest-bound evidence must not satisfy coverage")
}

// TestSourceInventoryRemainsAvailable guards the other half of the contract:
// image coverage is separate evidence, and adopting it must not cost the Go
// source inventory this agent already provides.
func TestSourceInventoryRemainsAvailable(t *testing.T) {
	ctx := context.Background()
	identity := goGrpcServiceFixture(t)
	client := startBuilderAgent(t, identity)

	resp, err := client.SBOM(ctx, &builderv0.SBOMRequest{})
	require.NoError(t, err)
	require.Equal(t, builderv0.SBOMStatus_COMPLETE, resp.GetState().GetState(), resp.GetState().GetMessage())
	require.Equal(t, builderv0.SBOMScope_SBOM_SCOPE_SOURCE, resp.GetScope())
	require.NotEmpty(t, resp.GetSha256())
}

// TestImageScopeWithoutSubjectsIsAPreconditionFailure proves the inherited
// image-scope path is reachable through the composed agent. This specialization
// overrides Load and Build and embeds the generic builder for the rest, so only
// the wire surface distinguishes an image-scope request that reaches the shared
// implementation from one that silently falls back to a source inventory.
func TestImageScopeWithoutSubjectsIsAPreconditionFailure(t *testing.T) {
	ctx := context.Background()
	identity := goGrpcServiceFixture(t)
	client := startBuilderAgent(t, identity)

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
func TestPlanDerivedSubjectsAreRefusedUntilTheyCarryADigest(t *testing.T) {
	ctx := context.Background()
	identity := goGrpcServiceFixture(t)
	client := startBuilderAgent(t, identity)

	plan := buildRecipePlan(t, ctx, client, identity)
	expected := sbom.ExpectedFromBuildPlan(resources.ServiceUnique(identity.GetModule(), identity.GetName()), plan)
	require.NotEmpty(t, expected)
	require.Empty(t, expected[0].GetDigest())

	resp, err := client.SBOM(ctx, &builderv0.SBOMRequest{
		Scope:    builderv0.SBOMScope_SBOM_SCOPE_IMAGE,
		Subjects: expected,
	})
	require.NoError(t, err)
	require.Equal(t, builderv0.SBOMStatus_ERROR, resp.GetState().GetState())
	require.Equal(t, builderv0.SBOMScope_SBOM_SCOPE_IMAGE, resp.GetScope())
	require.Error(t, sbom.ValidateCoverage(expected, resp))
}
