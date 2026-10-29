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
// pushes and removes only its own containers and uniquely tagged image.
func TestPackagingImage(t *testing.T) {
	if os.Getenv("CODEFLY_PACKAGING_IMAGE_TEST") != "1" {
		t.Skip("set CODEFLY_PACKAGING_IMAGE_TEST=1 for a real Docker build")
	}
	// CGO_ENABLED alone does not prove native compatibility: the CGO variants
	// call libc, and the package variants execute the declared OS command.
	for _, tc := range []struct {
		name     string
		cgo      bool
		packages []string
	}{
		{name: "static"},
		{name: "cgo", cgo: true},
		{name: "packages", packages: []string{"git"}},
		{name: "cgo_packages", cgo: true, packages: []string{"git"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testPackagingImage(t, tc.cgo, tc.packages)
		})
	}
}

func testPackagingImage(t *testing.T, cgo bool, packages []string) {
	identity := goGrpcServiceFixture(t)
	client, builder := startBuilderAgent(t, identity)
	root := filepath.Join(identity.GetWorkspacePath(), "mod/svc")
	for _, dir := range []string{"code/cmd/inspect", "routing", "base", "proto"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o755))
	}
	main := "package main\nimport (\"fmt\"; \"os\")\nfunc main() { b,e:=os.ReadFile(\"../routing/routes.json\"); if e!=nil {panic(e)}; fmt.Print(string(b)) }\n"
	if cgo {
		main = strings.Replace(main, "import (", "// #include <stdlib.h>\nimport \"C\"\nimport (", 1)
		main = strings.Replace(main, "func main() {", "func main() { if C.abs(-7) != 7 { panic(\"libc call failed\") };", 1)
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "code/main.go"), []byte(main), 0o644))
	inspect, err := os.ReadFile("testdata/packaging/inspect.go.txt")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "code/cmd/inspect/main.go"), inspect, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "code/go.sum"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "routing/routes.json"), []byte("runtime-assets-present"), 0o644))
	builder.GoGrpc.Settings.BuildCommands = []BuildCommand{{Name: "inspect", Package: "./cmd/inspect"}}
	builder.GoGrpc.Settings.RuntimeAssets = []string{"routing"}
	builder.GoGrpc.Settings.WithCGO = cgo
	builder.GoGrpc.Settings.RuntimePackages = packages
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
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		output, err := exec.CommandContext(cleanup, "docker", "image", "rm", image).CombinedOutput()
		require.NoError(t, err, "%s", output)
	})
	run := func(entrypoint string, command ...string) string {
		t.Helper()
		args := []string{"create", "--network=none"}
		if entrypoint != "" {
			args = append(args, "--entrypoint", entrypoint)
		}
		id := docker(append(append(args, image), command...)...)
		t.Logf("owned packaging container: %s", id)
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			output, err := exec.CommandContext(cleanup, "docker", "rm", "--force", id).CombinedOutput()
			require.NoError(t, err, "%s", output)
			output, err = exec.CommandContext(cleanup, "docker", "container", "inspect", id).CombinedOutput()
			require.Error(t, err)
			require.Contains(t, string(output), "No such container")
			t.Logf("owned packaging container removed and absent: %s", id)
		})
		output := docker("start", "--attach", id)
		require.Equal(t, "0", docker("inspect", "--format", "{{.State.ExitCode}}", id))
		return output
	}
	arch := docker("version", "--format", "{{.Server.Arch}}")
	require.Contains(t, []string{"amd64", "arm64"}, arch)
	docker("build", "--progress=plain", "--platform", "linux/"+arch, "--label", "codefly.packaging-test=true", "--build-arg", "TARGETARCH="+arch, "-t", image, "-f", filepath.Join(root, "builder", plan.Recipes[0].Dockerfile), root)
	// Preserve the built image even when a runtime assertion fails.
	if evidence := os.Getenv("CODEFLY_PACKAGING_EVIDENCE_DIR"); evidence != "" {
		dir := filepath.Join(evidence, arch, filepath.Base(t.Name()))
		require.NoError(t, os.MkdirAll(dir, 0o755))
		docker("image", "save", "--output", filepath.Join(dir, "image.tar"), image)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "image-inspect.json"), []byte(docker("image", "inspect", image)), 0o644))
	}

	require.Equal(t, "runtime-assets-present", run(""))
	t.Logf("built image config digest: %s; platform: linux/%s", docker("image", "inspect", "--format", "{{.Id}}", image), arch)
	mode := "scratch"
	if cgo || len(packages) > 0 {
		mode = "native"
	}
	metadata := run("inspect", mode)
	if len(packages) > 0 {
		require.Contains(t, run("git", "--version"), "git version ")
	}

	require.Contains(t, metadata, "command-present\n")
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
	output, err = run("create", "--network=none", image)
	require.NoError(t, err)
	id := strings.TrimSpace(output)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		output, err := exec.CommandContext(cleanup, "docker", "rm", "--force", id).CombinedOutput()
		require.NoError(t, err, "%s", output)
	})
	metadataPath := filepath.Join(t.TempDir(), "binaries.txt")
	output, err = run("cp", id+":/usr/share/codefly/build/binaries.txt", metadataPath)
	require.NoError(t, err, "%s", output)
	metadata, err := os.ReadFile(metadataPath)
	require.NoError(t, err)
	require.Contains(t, string(metadata), strings.TrimPrefix(pkg, "./"))
}
