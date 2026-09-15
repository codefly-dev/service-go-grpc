package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

// This opt-in integration calls Build over its public RPC, builds that exact
// recipe, and executes both the main program and a declared command. It never
// pushes and removes only the image created under this test's unique tag.
func TestPackagingImage(t *testing.T) {
	if os.Getenv("CODEFLY_PACKAGING_IMAGE_TEST") != "1" {
		t.Skip("set CODEFLY_PACKAGING_IMAGE_TEST=1 for a real Docker build")
	}
	identity := goGrpcServiceFixture(t)
	client, builder := startBuilderAgent(t, identity)
	root := filepath.Join(identity.GetWorkspacePath(), "mod/svc")
	for _, dir := range []string{"code/cmd/inspect", "routing", "base", "proto"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o755))
	}
	main := "package main\nimport (\"fmt\"; \"os\")\nfunc main() { b,e:=os.ReadFile(\"../routing/routes.json\"); if e!=nil {panic(e)}; fmt.Print(string(b)) }\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "code/main.go"), []byte(main), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "code/cmd/inspect/main.go"), []byte("package main\nimport \"fmt\"\nfunc main(){fmt.Print(\"command-present\")}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "code/go.sum"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "routing/routes.json"), []byte("runtime-assets-present"), 0o644))
	builder.GoGrpc.Settings.BuildCommands = []BuildCommand{{Name: "inspect", Package: "./cmd/inspect"}}
	builder.GoGrpc.Settings.RuntimeAssets = []string{"routing"}
	plan := buildRecipePlan(context.Background(), t, client, identity)
	require.Len(t, plan.Recipes, 1)
	image := fmt.Sprintf("codefly-packaging-test:%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	docker := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "docker", args...)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return strings.TrimSpace(string(output))
	}
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", image).Run() })
	arch := docker("version", "--format", "{{.Server.Arch}}")
	require.Contains(t, []string{"amd64", "arm64"}, arch)
	docker("build", "--progress=plain", "--platform", "linux/"+arch, "--label", "codefly.packaging-test=true", "--build-arg", "TARGETARCH="+arch, "-t", image, "-f", filepath.Join(root, "builder", plan.Recipes[0].Dockerfile), root)
	require.Equal(t, "runtime-assets-present", docker("run", "--rm", "--network=none", image))
	require.Equal(t, "command-present", docker("run", "--rm", "--network=none", "--entrypoint", "inspect", image))
	docker("run", "--rm", "--network=none", "--entrypoint", "sh", image, "-c", "test -s /usr/share/codefly/build/modules.json && test -s /usr/share/codefly/build/binaries.txt && test -s /usr/share/codefly/build/sources.sha256 && test -f /usr/share/codefly/build/go.sum && test $(id -u) != 0 && sha256sum -c /usr/share/codefly/build/runtime.sha256")
	metadata := docker("run", "--rm", "--network=none", "--entrypoint", "cat", image, "/usr/share/codefly/build/binaries.txt")
	require.Contains(t, metadata, "example.com/svc/cmd/inspect")
}

func TestRuntimeAssetsRefuseMissingAndSymlinkFiles(t *testing.T) {
	root := t.TempDir()
	require.Error(t, validateRuntimeAssetSources(root, []string{"routing"}))
	require.NoError(t, os.Mkdir(filepath.Join(root, "routing"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "routing/routes.json"), []byte("{}"), 0o644))
	require.NoError(t, validateRuntimeAssetSources(root, []string{"routing"}))
	require.NoError(t, os.Symlink("/missing", filepath.Join(root, "routing/link")))
	require.Error(t, validateRuntimeAssetSources(root, []string{"routing"}))
}

func TestRuntimeAssetsBuildRefusalOverGRPC(t *testing.T) {
	identity := goGrpcServiceFixture(t)
	client, builder := startBuilderAgent(t, identity)
	builder.GoGrpc.Settings.RuntimeAssets = []string{"routing"}
	root := filepath.Join(identity.GetWorkspacePath(), "mod/svc")
	request := &builderv0.BuildRequest{
		OutputDirectory: filepath.Join(root, "builder"),
		BuildContext: &builderv0.BuildContext{Kind: &builderv0.BuildContext_DockerBuildContext{
			DockerBuildContext: &builderv0.DockerBuildContext{DockerRepository: "registry.example.com"},
		}},
	}
	response, err := client.Build(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_ERROR, response.GetState().GetState())
	require.Contains(t, response.GetState().GetMessage(), "runtime asset")
	require.Nil(t, response.GetResult().GetDockerBuildPlan())
	require.NoDirExists(t, request.OutputDirectory)
}

// A selected real service can exercise this same Build RPC without modifying its
// pinned manifest. The command declaration is prospective generator input until
// the service's owner adopts a released generator in its authored topology.
func TestPackagingOwnerCommandImage(t *testing.T) {
	source := os.Getenv("CODEFLY_PACKAGING_SERVICE_ROOT")
	if source == "" {
		t.Skip("set CODEFLY_PACKAGING_SERVICE_ROOT and command selection for owner image proof")
	}
	name, pkg := os.Getenv("CODEFLY_PACKAGING_COMMAND_NAME"), os.Getenv("CODEFLY_PACKAGING_COMMAND_PACKAGE")
	command := BuildCommand{Name: name, Package: pkg}
	require.NoError(t, validateBuildCommands([]BuildCommand{command}))
	identity := goGrpcServiceFixture(t)
	root := filepath.Join(identity.GetWorkspacePath(), "mod/svc")
	// Remove only the two files this test fixture created, then copy the real
	// reviewed service code. No source file is modified or credential read.
	require.NoError(t, os.Remove(filepath.Join(root, "code/main.go")))
	require.NoError(t, os.Remove(filepath.Join(root, "code/go.mod")))
	require.NoError(t, os.CopyFS(filepath.Join(root, "code"), os.DirFS(filepath.Join(source, "code"))))
	client, builder := startBuilderAgent(t, identity)
	builder.GoGrpc.Settings.BuildCommands = []BuildCommand{command}
	plan := buildRecipePlan(context.Background(), t, client, identity)
	image := fmt.Sprintf("codefly-owner-packaging-test:%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "docker", args...)
		output, err := cmd.CombinedOutput()
		return string(output), err
	}
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", image).Run() })
	arch, err := run("version", "--format", "{{.Server.Arch}}")
	require.NoError(t, err)
	arch = strings.TrimSpace(arch)
	require.Contains(t, []string{"arm64", "amd64"}, arch)
	output, err := run("build", "--progress=plain", "--platform", "linux/"+arch, "--build-arg", "TARGETARCH="+arch, "--label", "codefly.packaging-test=true", "-t", image, "-f", filepath.Join(root, "builder", plan.Recipes[0].Dockerfile), root)
	require.NoError(t, err, "%s", output)
	// Help exercises the actual executable loader without starting dependencies.
	output, err = run("run", "--rm", "--network=none", "--entrypoint", name, image, "-h")
	require.Contains(t, output, "Usage of "+name)
	if err != nil {
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit)
		require.Equal(t, 2, exit.ExitCode(), output)
	}
	output, err = run("run", "--rm", "--network=none", "--entrypoint", "cat", image, "/usr/share/codefly/build/binaries.txt")
	require.NoError(t, err)
	require.Contains(t, output, strings.TrimPrefix(pkg, "./"))
}
