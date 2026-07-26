package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageGeneratedTree lays out the #30 repro: a service module "acc" whose
// grpc-gateway output references request types from a sibling proto package
// (jobsv1, declared in a directory whose tail segment is v1) without importing
// it, while the companion .pb.go imports the same package under a different
// alias. Returns the staged root and the path of the broken gateway file.
func stageGeneratedTree(t *testing.T) (string, string) {
	t.Helper()
	stage := t.TempDir()
	writeTestFile(t, filepath.Join(stage, "code", "pkg", "gen", "saas", "jobs", "v1", "jobs.pb.go"),
		"package jobsv1\n\ntype GetJobRequest struct{}\ntype ListJobsRequest struct{}\n")
	writeTestFile(t, filepath.Join(stage, "code", "pkg", "gen", "saas", "accounts", "v1", "platform_admin.pb.go"),
		"package accountsv1\n\nimport v1 \"acc/pkg/gen/saas/jobs/v1\"\n\nvar _ = v1.GetJobRequest{}\n")
	gateway := filepath.Join(stage, "code", "pkg", "gen", "saas", "accounts", "v1", "platform_admin.pb.gw.go")
	writeTestFile(t, gateway,
		"package accountsv1\n\nfunc handle() {\n\tvar protoReq jobsv1.GetJobRequest\n\tvar list jobsv1.ListJobsRequest\n\t_, _ = protoReq, list\n}\n")
	return stage, gateway
}

func writeGoMod(t *testing.T, stage, modulePath string) string {
	t.Helper()
	goMod := filepath.Join(stage, "code", "go.mod")
	writeTestFile(t, goMod, "module "+modulePath+"\n\ngo 1.26\n")
	return goMod
}

// TestAddGeneratedCrossPackageImportsRepairsGatewayFile proves the missing
// cross-package import is inserted so the generated code compiles, and that a
// second pass — and the goimports pass that follows in Sync — leave it byte
// stable, so sync-drift and compile agree.
func TestAddGeneratedCrossPackageImportsRepairsGatewayFile(t *testing.T) {
	stage, gateway := stageGeneratedTree(t)
	goMod := writeGoMod(t, stage, "acc")

	if err := addGeneratedCrossPackageImports(stage, goMod, "code", []string{"code/pkg/gen"}); err != nil {
		t.Fatal(err)
	}
	repaired, err := os.ReadFile(gateway)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(repaired), `jobsv1 "acc/pkg/gen/saas/jobs/v1"`) {
		t.Fatalf("cross-package import not added:\n%s", repaired)
	}

	// The goimports pass Sync runs next must find nothing more to change, and a
	// second repair pass must be a no-op: both are what keep sync-drift clean.
	if err := formatStagedGo(stage); err != nil {
		t.Fatal(err)
	}
	formatted, err := os.ReadFile(gateway)
	if err != nil {
		t.Fatal(err)
	}
	if err := addGeneratedCrossPackageImports(stage, goMod, "code", []string{"code/pkg/gen"}); err != nil {
		t.Fatal(err)
	}
	assertTestFile(t, gateway, string(formatted))
}

// TestAddGeneratedCrossPackageImportsLeavesCorrectFilesUntouched proves the
// pass never rewrites a file whose references are already imported.
func TestAddGeneratedCrossPackageImportsLeavesCorrectFilesUntouched(t *testing.T) {
	stage, _ := stageGeneratedTree(t)
	goMod := writeGoMod(t, stage, "acc")
	sibling := filepath.Join(stage, "code", "pkg", "gen", "saas", "accounts", "v1", "platform_admin.pb.go")
	before, err := os.ReadFile(sibling)
	if err != nil {
		t.Fatal(err)
	}

	if err := addGeneratedCrossPackageImports(stage, goMod, "code", []string{"code/pkg/gen"}); err != nil {
		t.Fatal(err)
	}
	assertTestFile(t, sibling, string(before))
}

// TestAddGeneratedCrossPackageImportsSkipsWithoutGoMod proves the pass leaves
// the tree exactly as generated when the module path cannot be resolved (e.g.
// before go.mod exists during service creation), rather than failing the sync.
func TestAddGeneratedCrossPackageImportsSkipsWithoutGoMod(t *testing.T) {
	stage, gateway := stageGeneratedTree(t)
	before, err := os.ReadFile(gateway)
	if err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(stage, "code", "go.mod")
	if err := addGeneratedCrossPackageImports(stage, missing, "code", []string{"code/pkg/gen"}); err != nil {
		t.Fatal(err)
	}
	assertTestFile(t, gateway, string(before))
}

// TestAddGeneratedCrossPackageImportsReportsUnreadableGoMod proves a go.mod
// that exists but cannot be read is surfaced as an error rather than silently
// disabling the repair — only a missing go.mod is a valid skip. A directory at
// the go.mod path yields a non-IsNotExist read error on every platform.
func TestAddGeneratedCrossPackageImportsReportsUnreadableGoMod(t *testing.T) {
	stage, _ := stageGeneratedTree(t)
	goModDir := filepath.Join(stage, "code", "go.mod")
	if err := os.MkdirAll(goModDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := addGeneratedCrossPackageImports(stage, goModDir, "code", []string{"code/pkg/gen"}); err == nil {
		t.Fatal("unreadable go.mod was silently skipped instead of reported")
	}
}

// TestAddGeneratedCrossPackageImportsSkipsOwnPackageSelector proves a selector
// that names the file's own package is not turned into a self-import, which
// would not compile.
func TestAddGeneratedCrossPackageImportsSkipsOwnPackageSelector(t *testing.T) {
	stage := t.TempDir()
	goMod := writeGoMod(t, stage, "acc")
	// A file in package accountsv1 that qualifies a name with its own package.
	own := filepath.Join(stage, "code", "pkg", "gen", "saas", "accounts", "v1", "self.go")
	writeTestFile(t, own, "package accountsv1\n\nvar Value = accountsv1.Thing\n")
	before, err := os.ReadFile(own)
	if err != nil {
		t.Fatal(err)
	}

	if err := addGeneratedCrossPackageImports(stage, goMod, "code", []string{"code/pkg/gen"}); err != nil {
		t.Fatal(err)
	}
	assertTestFile(t, own, string(before))
}

// TestAddGeneratedCrossPackageImportsAddsMultiplePackages proves a file missing
// references to two different generated packages receives both imports and, run
// through the goimports pass Sync applies next, lands byte-stable regardless of
// the order the imports were inserted.
func TestAddGeneratedCrossPackageImportsAddsMultiplePackages(t *testing.T) {
	stage := t.TempDir()
	goMod := writeGoMod(t, stage, "acc")
	writeTestFile(t, filepath.Join(stage, "code", "pkg", "gen", "saas", "jobs", "v1", "jobs.pb.go"),
		"package jobsv1\n\ntype GetJobRequest struct{}\n")
	writeTestFile(t, filepath.Join(stage, "code", "pkg", "gen", "saas", "billing", "v1", "billing.pb.go"),
		"package billingv1\n\ntype GetInvoiceRequest struct{}\n")
	gateway := filepath.Join(stage, "code", "pkg", "gen", "saas", "accounts", "v1", "platform_admin.pb.gw.go")
	writeTestFile(t, gateway,
		"package accountsv1\n\nfunc handle() {\n\tvar job jobsv1.GetJobRequest\n\tvar inv billingv1.GetInvoiceRequest\n\t_, _ = job, inv\n}\n")

	if err := addGeneratedCrossPackageImports(stage, goMod, "code", []string{"code/pkg/gen"}); err != nil {
		t.Fatal(err)
	}
	if err := formatStagedGo(stage); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(gateway)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`jobsv1 "acc/pkg/gen/saas/jobs/v1"`, `billingv1 "acc/pkg/gen/saas/billing/v1"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing import %s in:\n%s", want, out)
		}
	}
	// A repeated pass converges on the same bytes: sync-drift stays clean.
	if err := addGeneratedCrossPackageImports(stage, goMod, "code", []string{"code/pkg/gen"}); err != nil {
		t.Fatal(err)
	}
	if err := formatStagedGo(stage); err != nil {
		t.Fatal(err)
	}
	assertTestFile(t, gateway, string(out))
}

// TestAddGeneratedCrossPackageImportsSkipsAmbiguousName proves a package name
// declared by two generated directories is not resolved: the reference could
// mean either import, so it is left for the lint to flag rather than guessed.
func TestAddGeneratedCrossPackageImportsSkipsAmbiguousName(t *testing.T) {
	stage, gateway := stageGeneratedTree(t)
	goMod := writeGoMod(t, stage, "acc")
	// A second directory also declaring package jobsv1 makes the name ambiguous.
	writeTestFile(t, filepath.Join(stage, "code", "pkg", "gen", "saas", "jobs", "v2", "jobs.pb.go"),
		"package jobsv1\n\ntype GetJobRequest struct{}\n")
	before, err := os.ReadFile(gateway)
	if err != nil {
		t.Fatal(err)
	}

	if err := addGeneratedCrossPackageImports(stage, goMod, "code", []string{"code/pkg/gen"}); err != nil {
		t.Fatal(err)
	}
	assertTestFile(t, gateway, string(before))
}
