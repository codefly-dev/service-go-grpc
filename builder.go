package main

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	goast "go/ast"
	"go/format"
	goparser "go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/imports"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
	"github.com/codefly-dev/core/agents/communicate"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/agents/services/upgrade"
	"github.com/codefly-dev/core/companions/proto"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/resources"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/wool"

	gobuilder "github.com/codefly-dev/service-go/pkg/builder"
)

// Builder is the gRPC specialization of the generic Go Builder.
//
// Embedding chain:
//
//	*gobuilder.Builder  — promotes Init, Update from generic, plus the
//	                      services.Base chain (Wool, Logger, Location,
//	                      Identity, Endpoints, Templates, …) through
//	                      *goservice.Service.
//	GoGrpc *Service     — go-grpc-specific state: richer Settings and
//	                      the three protocol endpoints.
//
// Inherited: Init, Update.
// Overridden: Load (adds gRPC/REST/Connect endpoint discovery), Sync
// (buf proto regen + dependency gRPC codegen), Build (custom Docker
// templating with SourceDir/ModuleRoot/BuildTarget), Deploy (k8s),
// Create (five-question Communicate flow + CreateEndpoints).
type Builder struct {
	*gobuilder.Builder

	GoGrpc *Service

	answers map[string]*agentv0.Answer
}

// NewBuilder composes a go-grpc Builder by constructing a generic
// gobuilder.Builder with go-grpc's template FS and requirements.
func NewBuilder(svc *Service) *Builder {
	return &Builder{
		Builder: gobuilder.New(svc.Service, gobuilder.BuildConfig{
			FactoryFS:     factoryFS,
			BuilderFS:     builderFS,
			DeploymentFS:  deploymentFS,
			Requirements:  requirements,
			GoVersion:     GoVersion,
			AlpineVersion: AlpineVersion,
		}),
		GoGrpc: svc,
	}
}

// Load delegates to the generic Builder.Load (handles SourceLocation,
// CreationMode → GettingStarted, endpoint loading) and then adds
// go-grpc-specific gRPC/REST endpoint discovery.
func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	// Generic handles SourceLocation / cacheLocation / CreationMode /
	// LoadEndpoints. After this call, s.Endpoints is populated.
	resp, err := s.Builder.Load(ctx, req)
	if err != nil {
		return resp, err
	}

	// The generic Load unmarshalled settings into the GENERIC Settings struct,
	// which has no rest-endpoint / connect-endpoint fields — so those were
	// silently dropped. Re-unmarshal into the richer go-grpc Settings so the
	// endpoint discovery below sees them (mirrors Runtime.Load).
	if err := s.Base.Load(ctx, req.Identity, s.GoGrpc.Settings); err != nil {
		return nil, s.Wool.Wrapf(err, "cannot load go-grpc settings")
	}

	// Creation mode: generic returned early with GettingStarted.
	if req.CreationMode != nil {
		return resp, nil
	}

	// Discover the protocol endpoints. gRPC is mandatory.
	s.GoGrpc.GrpcEndpoint, err = resources.FindGRPCEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Base.Builder.LoadError(err)
	}
	if s.GoGrpc.Settings.RestEndpoint {
		s.GoGrpc.RestEndpoint, err = resources.FindRestEndpoint(ctx, s.Endpoints)
		if err != nil {
			return s.Base.Builder.LoadError(err)
		}
	}
	return resp, nil
}

// Init, Update are INHERITED from *gobuilder.Builder.

// Sync stages every generator-owned artifact, then either reports or applies
// the exact deterministic file set. Buf remains the only protocol generator;
// Go, Rust, and TypeScript outputs are declared together in buf.gen.yaml.
func (s *Builder) Sync(ctx context.Context, request *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if err := s.GoGrpc.Settings.Validate(); err != nil {
		return s.Base.Builder.SyncError(err)
	}
	transaction, err := newSyncTransaction(s.Location, s.Identity.RelativeToWorkspace)
	if err != nil {
		return s.Base.Builder.SyncError(err)
	}
	defer func() { _ = transaction.Close() }()

	// Protocol input and output paths are service-root-relative, not derived
	// from the Go module root. This keeps the conventional proto/ + code/
	// layout while allowing an explicit nested source directory for existing
	// services. moduleRoot only scopes generated Go dependency stubs below.
	moduleRoot, _ := golanghelpers.SplitSourceDir(s.GoGrpc.Settings.GoSourceDir())
	protoDir := s.GoGrpc.Settings.protocolSourceDir()
	if err := transaction.CopyInput(protoDir); err != nil {
		return s.Base.Builder.SyncError(err)
	}
	if err := redirectEscapingBufOutputs(transaction.StageRoot(), protoDir); err != nil {
		return s.Base.Builder.SyncError(err)
	}

	scaffoldTargets, err := generatedScaffoldTargets(s.Location, filepath.Join(s.Location, protoDir), s.Information.Service.Name.Title+"Service")
	if err != nil {
		return s.Base.Builder.SyncError(err)
	}
	if len(scaffoldTargets) > 0 {
		create := CreateConfiguration{Information: s.Information, Settings: s.GoGrpc.Settings, Envs: []string{}}
		generated := services.WithFactory(factoryFS).
			WithPathSelect(generatedScaffoldSelect()).
			WithOverride(shared.OverrideAll()).
			WithDestination("%s", transaction.StageRoot())
		if err := s.Templates(ctx, create, generated); err != nil {
			return s.Base.Builder.SyncError(err)
		}
		for _, target := range scaffoldTargets {
			if err := transaction.TrackFile(target); err != nil {
				return s.Base.Builder.SyncError(err)
			}
		}
	}

	bufRoot := filepath.Dir(filepath.Join(transaction.StageRoot(), protoDir))
	buf, err := proto.NewBuf(ctx, bufRoot)
	if err != nil {
		return s.Base.Builder.SyncError(err)
	}
	// Cache generation inputs outside the source tree but across transactions.
	// A transaction-local cache disappears after every Sync, forcing repeated
	// BSR requests even when proto inputs are unchanged.
	generationCache := filepath.Join(s.Location, ".codefly", "cache")
	if err := os.MkdirAll(generationCache, 0o755); err != nil {
		return s.Base.Builder.SyncError(fmt.Errorf("create generation cache: %w", err))
	}
	buf.WithCache(generationCache)
	for _, relative := range s.GoGrpc.Settings.protocolOutputDirs() {
		// Preserve the current generated tree in the transaction. On a cache
		// hit Buf intentionally does no work; without this baseline the staged
		// output would be absent and downstream formatting/comparison would
		// either fail or interpret the cache hit as deletion of every artifact.
		actualOutput := filepath.Join(s.Location, relative)
		if _, err := os.Lstat(actualOutput); err == nil {
			if err := transaction.CopyInput(relative); err != nil {
				return s.Base.Builder.SyncError(err)
			}
		} else if !os.IsNotExist(err) {
			return s.Base.Builder.SyncError(fmt.Errorf("stat generated output %q: %w", relative, err))
		}
		if err := transaction.TrackDirectory(relative); err != nil {
			return s.Base.Builder.SyncError(err)
		}
		buf.WithGeneratedDirs(filepath.Join(transaction.StageRoot(), relative))
	}
	if err := transaction.TrackFile(filepath.Join(protoDir, "buf.lock")); err != nil {
		return s.Base.Builder.SyncError(err)
	}
	if err := buf.Generate(ctx); err != nil {
		return s.Base.Builder.SyncError(err)
	}

	s.Wool.Debug("dependencies", wool.Field("dependencies", s.Base.Service.ServiceDependencies))
	for _, dep := range s.Base.Service.ServiceDependencies {
		ep, err := resources.FindGRPCEndpointFromService(ctx, dep, s.DependencyEndpoints)
		if err != nil {
			return s.Base.Builder.SyncError(err)
		}
		if ep == nil {
			continue
		}
		destination := filepath.Join(moduleRoot, "external", dep.Unique())
		if err := transaction.TrackDirectory(destination); err != nil {
			return s.Base.Builder.SyncError(err)
		}
		if err := proto.GenerateGRPC(ctx, languages.GO, filepath.Join(transaction.StageRoot(), destination), dep.Unique(), ep); err != nil {
			return s.Base.Builder.SyncError(err)
		}
	}

	// protoc-gen-grpc-gateway emits `pkg.RequestType` for request types declared
	// in another proto package but omits the import, so the generated .pb.gw.go
	// does not compile. goimports below cannot repair it: its candidate scan keys
	// on the import path's last segment (v1), which never matches the package's
	// own name (jobsv1). Resolve those references against the freshly generated
	// tree and insert the missing imports before the goimports pass sorts them.
	moduleImports := make([]string, 0, len(s.GoGrpc.Settings.protocolOutputDirs()))
	for _, relative := range s.GoGrpc.Settings.protocolOutputDirs() {
		if relative == moduleRoot || pathContains(moduleRoot, relative) {
			moduleImports = append(moduleImports, relative)
		}
	}
	if err := addGeneratedCrossPackageImports(transaction.StageRoot(), filepath.Join(s.Location, moduleRoot, "go.mod"), moduleRoot, moduleImports); err != nil {
		return s.Base.Builder.SyncError(err)
	}

	// buf and the language plugins emit Go that the agent's own lint
	// (corecode.GoCodeServer, golang.org/x/tools/imports) can still flag as
	// needing a safe fix, because the two paths pinned different goimports
	// versions. Run the exact function the lint runs — same x/tools version, so
	// byte-identical output — over the staged tree so generated code is
	// lint-clean and sync-drift and lint agree on it.
	if err := formatStagedGo(transaction.StageRoot()); err != nil {
		return s.Base.Builder.SyncError(err)
	}

	changed, err := transaction.ChangedFiles()
	if err != nil {
		return s.Base.Builder.SyncError(err)
	}
	if !request.GetDryRun() {
		if err := transaction.Apply(); err != nil {
			return s.Base.Builder.SyncError(err)
		}
	}
	response, err := s.Base.Builder.SyncResponse()
	if err != nil {
		return response, err
	}
	response.ChangedFiles = changed
	return response, nil
}

// formatStagedGo rewrites every staged .go file through the same goimports pass
// the lint uses to decide a file "needs Fix". Applied to the generated output
// before it is compared and swapped in, so the code the agent produces already
// satisfies the agent's own formatting check. imports.Process leaves an
// already-clean file untouched, so this is idempotent and keeps sync
// deterministic.
//
// Formatting at the stage path is a valid stand-in for the lint's later run at
// the real path because imports.Process is path-independent for this input:
// with no LocalPrefix and no missing/unused imports it reduces to gofmt plus a
// std/non-std import sort, neither of which depends on the file's location.
// Generated protobuf can reference a cross-package type without importing it
// (#30); addGeneratedCrossPackageImports runs first and inserts those imports,
// so the no-missing-imports precondition holds by the time this pass runs.
func formatStagedGo(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// Buf leaves a private .cache directory in the staging root. A
		// containerized generator can create it as a foreign UID, leaving it
		// unreadable; descending into it would abort a non-mutating sync with a
		// permission error. It never holds generator-owned .go output, so skip
		// it before WalkDir tries to read its entries.
		if entry.IsDir() && entry.Name() == syncCacheDir {
			return filepath.SkipDir
		}
		// Only regular .go files: a symlink ending in .go would be dereferenced
		// by the os.WriteFile below and truncate its target, possibly outside the
		// staged tree.
		if !entry.Type().IsRegular() || filepath.Ext(path) != ".go" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// A file goimports cannot parse is left untouched rather than failing the
		// whole sync — the lint reports it separately, and sync has no more to add
		// than it did before this pass existed.
		fixed, err := imports.Process(path, content, &imports.Options{Comments: true})
		if err != nil {
			return nil
		}
		if bytes.Equal(content, fixed) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(path, fixed, info.Mode().Perm())
	})
}

// addGeneratedCrossPackageImports inserts imports that a generated Go file
// references but does not declare — the protoc-gen-grpc-gateway cross-package
// request-type bug (#30). Every such reference resolves to a sibling package in
// the same freshly generated tree, so goOutputDirs are scanned twice: once to
// index each generated package by its declared name, then once more to add the
// missing import to any file that names a package it never imported. goModPath
// supplies the module path that turns a staged directory into an import path;
// when go.mod does not exist yet (e.g. during service creation) the repair is
// skipped, leaving the tree exactly as generated.
func addGeneratedCrossPackageImports(stageRoot, goModPath, moduleRoot string, goOutputDirs []string) error {
	modContent, err := os.ReadFile(goModPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	modulePath := modfile.ModulePath(modContent)
	if modulePath == "" {
		return nil
	}
	byName, err := generatedPackagePaths(stageRoot, filepath.Join(stageRoot, moduleRoot), modulePath, goOutputDirs)
	if err != nil {
		return err
	}
	for _, relative := range goOutputDirs {
		err := filepath.WalkDir(filepath.Join(stageRoot, relative), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !entry.Type().IsRegular() || filepath.Ext(path) != ".go" {
				return nil
			}
			return insertMissingImports(path, byName)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// generatedPackagePaths maps each generated package's declared name to its
// import path. A name declared by two packages is dropped: a bare reference to
// it cannot be resolved to a single import, so it is left for the lint to flag.
func generatedPackagePaths(stageRoot, stageModuleRoot, modulePath string, goOutputDirs []string) (map[string]string, error) {
	byName := map[string]string{}
	ambiguous := map[string]struct{}{}
	for _, relative := range goOutputDirs {
		err := filepath.WalkDir(filepath.Join(stageRoot, relative), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !entry.Type().IsRegular() || filepath.Ext(path) != ".go" {
				return nil
			}
			file, err := goparser.ParseFile(token.NewFileSet(), path, nil, goparser.PackageClauseOnly)
			if err != nil {
				return nil
			}
			name := file.Name.Name
			rel, err := filepath.Rel(stageModuleRoot, filepath.Dir(path))
			if err != nil {
				return err
			}
			importPath := modulePath + "/" + filepath.ToSlash(rel)
			if existing, ok := byName[name]; ok && existing != importPath {
				ambiguous[name] = struct{}{}
			}
			byName[name] = importPath
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for name := range ambiguous {
		delete(byName, name)
	}
	return byName, nil
}

// insertMissingImports adds an import for every generated package the file
// names through a selector but never imports. A selector prefix is treated as a
// package only when it matches a generated package name that is neither the
// file's own package nor already in its import block; the following goimports
// pass drops the import again if a local of the same name shadows it, so no
// scope analysis is needed here.
func insertMissingImports(path string, byName map[string]string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, path, content, goparser.ParseComments)
	if err != nil {
		return nil
	}
	resolved := map[string]struct{}{file.Name.Name: {}}
	for _, spec := range file.Imports {
		resolved[importSpecName(spec)] = struct{}{}
	}
	missing := map[string]string{}
	goast.Inspect(file, func(node goast.Node) bool {
		selector, ok := node.(*goast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := selector.X.(*goast.Ident)
		if !ok {
			return true
		}
		if _, ok := resolved[ident.Name]; ok {
			return true
		}
		if importPath, ok := byName[ident.Name]; ok {
			missing[ident.Name] = importPath
		}
		return true
	})
	if len(missing) == 0 {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	for name, importPath := range missing {
		astutil.AddNamedImport(fset, file, name, importPath)
	}
	var buffer bytes.Buffer
	if err := format.Node(&buffer, fset, file); err != nil {
		return err
	}
	if bytes.Equal(content, buffer.Bytes()) {
		return nil
	}
	return os.WriteFile(path, buffer.Bytes(), info.Mode().Perm())
}

func importSpecName(spec *goast.ImportSpec) string {
	if spec.Name != nil {
		return spec.Name.Name
	}
	path := strings.Trim(spec.Path.Value, `"`)
	return path[strings.LastIndex(path, "/")+1:]
}

// generatedScaffoldSelect traverses only the directories required to reach
// generated service plumbing and selects only generator-owned Go files.
func generatedScaffoldSelect() shared.PathSelect {
	return generatedScaffoldSelection{}
}

type generatedScaffoldSelection struct{}

func (generatedScaffoldSelection) Keep(name string) bool {
	switch name {
	case "code", "pkg", "adapters", "plugins", "main.go.tmpl":
		return true
	default:
		return filepath.Ext(name) == ".tmpl" && filepath.Base(name) != "rpcs.go.tmpl" && bytes.HasSuffix([]byte(name), []byte("_gen.go.tmpl"))
	}
}

func generatedScaffoldTargets(root, protoRoot, expectedService string) ([]string, error) {
	mainPath := filepath.Join(root, "code", "main.go")
	contents, err := os.ReadFile(mainPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(contents, []byte("This code is generated by the agent")) {
		return nil, nil
	}
	services, err := declaredProtoServices(protoRoot)
	if err != nil {
		return nil, err
	}
	if len(services) != 1 || services[0] != expectedService {
		return nil, nil
	}
	return []string{
		filepath.Join("code", "main.go"),
		filepath.Join("code", "pkg", "adapters", "connect_gen.go"),
		filepath.Join("code", "pkg", "adapters", "cors_gen.go"),
		filepath.Join("code", "pkg", "adapters", "grpc_gen.go"),
		filepath.Join("code", "pkg", "adapters", "rest_gen.go"),
		filepath.Join("code", "pkg", "adapters", "server_gen.go"),
		filepath.Join("code", "plugins", "registry_gen.go"),
	}, nil
}

func declaredProtoServices(root string) ([]string, error) {
	var services []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".proto" {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		node, parseErr := parser.Parse(path, file, reporter.NewHandler(nil))
		closeErr := file.Close()
		if parseErr != nil {
			return parseErr
		}
		if closeErr != nil {
			return closeErr
		}
		for _, declaration := range node.Decls {
			if service, ok := declaration.(*ast.ServiceNode); ok {
				services = append(services, service.Name.Val)
			}
		}
		return nil
	})
	return services, err
}

// Build produces the service's Docker image. Uses the BuildGoDocker
// helper with a custom DockerTemplating hook to split the Go source
// directory into module root + build target — go-grpc services may nest
// their main package under cmd/server rather than at the module root.
func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	return golanghelpers.BuildGoDocker(ctx, s.Base.Builder, req, s.Location,
		requirements, builderFS, GoVersion, AlpineVersion,
		func(d *golanghelpers.DockerTemplating) {
			sourceDir := s.GoGrpc.Settings.GoSourceDir()
			d.SourceDir = sourceDir
			d.ModuleRoot, d.BuildTarget = golanghelpers.SplitSourceDir(sourceDir)
		})
}

// Upgrade bumps Go module dependencies (go get -u=patch by default,
// go get -u with --major, then go mod tidy). Runs at the Go source root.
func (s *Builder) Upgrade(ctx context.Context, req *builderv0.UpgradeRequest) (*builderv0.UpgradeResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	dir := s.Local("%s", s.GoGrpc.Settings.GoSourceDir())
	res, err := upgrade.Golang(ctx, dir, upgrade.Options{
		IncludeMajor: req.IncludeMajor,
		DryRun:       req.DryRun,
		Only:         req.Only,
	})
	if err != nil {
		return s.Base.Builder.UpgradeError(err)
	}
	return s.Base.Builder.UpgradeResponse(res.Changes, res.LockfileDiff)
}

// Deploy applies the k8s manifests in templates/deployment.
func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	return golanghelpers.DeployGoKubernetes(ctx, s.Base.Builder, req, s.EnvironmentVariables, deploymentFS)
}

// CreateEndpoints materializes gRPC / REST / Connect Endpoint resources
// from the proto and openapi descriptors scaffolded by Create.
func (s *Builder) CreateEndpoints(ctx context.Context) error {
	grpc, err := resources.LoadGrpcAPI(ctx, shared.Pointer(s.Local(standards.ProtoPath)))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot load grpc api")
	}
	endpoint := s.Base.BaseEndpoint(standards.GRPC)
	s.GoGrpc.GrpcEndpoint, err = resources.NewAPI(ctx, endpoint, resources.ToGrpcAPI(grpc))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create grpc api")
	}
	s.Endpoints = append(s.Endpoints, s.GoGrpc.GrpcEndpoint)

	if s.GoGrpc.Settings.RestEndpoint {
		rest, err := resources.LoadRestAPI(ctx, shared.Pointer(s.Local(standards.OpenAPIPath)))
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create openapi api")
		}
		endpoint = s.Base.BaseEndpoint(standards.REST)
		s.GoGrpc.RestEndpoint, err = resources.NewAPI(ctx, endpoint, resources.ToRestAPI(rest))
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create openapi api")
		}
		s.Endpoints = append(s.Endpoints, s.GoGrpc.RestEndpoint)
	}

	if s.GoGrpc.Settings.ConnectEndpoint {
		endpoint = s.Base.BaseEndpoint(standards.CONNECT)
		s.GoGrpc.ConnectEndpoint, err = resources.NewAPI(ctx, endpoint, resources.ToHTTPAPI(&basev0.HttpAPI{}))
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create connect api")
		}
		// NewAPI maps HttpAPI → "http"; override to the actual protocol tag.
		s.GoGrpc.ConnectEndpoint.Api = standards.CONNECT
		s.Endpoints = append(s.Endpoints, s.GoGrpc.ConnectEndpoint)
	}
	return nil
}

// Options returns the five-question set shown during `codefly add service`.
func (s *Builder) Options() []*agentv0.Question {
	return []*agentv0.Question{
		communicate.NewConfirm(&agentv0.Message{Name: HotReload, Message: "Code hot-reload (Recommended)?", Description: "codefly can restart your service when code changes are detected 🔎"}, true),
		communicate.NewConfirm(&agentv0.Message{Name: DebugSymbols, Message: "Start with debug symbols?", Description: "Build the go binary with debug symbol to use stack debugging"}, false),
		communicate.NewConfirm(&agentv0.Message{Name: RaceConditionDetectionRun, Message: "Start with race condition detection?", Description: "Build the go binary with race condition detection"}, false),
		communicate.NewConfirm(&agentv0.Message{Name: RestEndpointSetting, Message: "Automatic REST generation (Recommended)?", Description: "codefly can generate a REST server that stays magically 🪄 synced to your gRPC definition -- the easiest way to do REST"}, true),
		communicate.NewConfirm(&agentv0.Message{Name: ConnectEndpointSetting, Message: "Connect endpoint (browser-native RPC)?", Description: "Expose a ConnectRPC endpoint for type-safe browser clients — serves Connect, gRPC, and gRPC-Web on a single port"}, false),
	}
}

// CreateConfiguration is the template context passed to factory templates.
type CreateConfiguration struct {
	*services.Information
	Settings *Settings
	Envs     []string
}

// Create applies factory templates and creates the gRPC endpoint resources.
// Overrides generic Create: go-grpc scaffolding needs .proto preservation
// and endpoint creation after template application.
func (s *Builder) Create(ctx context.Context, _ *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if s.Base.Builder.CreationMode != nil && s.Base.Builder.CreationMode.Communicate && s.answers != nil {
		if err := s.populateSettingsFromAnswers(); err != nil {
			return s.Base.Builder.CreateError(err)
		}
	} else {
		if err := s.populateSettingsFromDefaults(); err != nil {
			return s.Base.Builder.CreateError(err)
		}
	}

	create := CreateConfiguration{Information: s.Information, Settings: s.GoGrpc.Settings, Envs: []string{}}
	ignore := shared.NewIgnore("go.work*", "service.generation.codefly.yaml")
	override := shared.OverrideException(shared.NewIgnore("*.proto"))

	if err := s.Templates(ctx, create, services.WithFactory(factoryFS).WithPathSelect(ignore).WithOverride(override)); err != nil {
		return s.Base.Builder.CreateError(err)
	}

	// Create must leave a service in the same canonical state as Sync. The
	// factory contains readable starter artifacts, while Buf and the generated
	// scaffold transaction own the authoritative protocol output. Running that
	// transaction before returning prevents a newly added service from failing
	// the very first sync-drift gate.
	syncResponse, err := s.Sync(ctx, &builderv0.SyncRequest{})
	if err != nil {
		return s.Base.Builder.CreateError(s.Wool.Wrapf(err, "synchronize generated service"))
	}
	if err := syncResponseError(syncResponse); err != nil {
		return s.Base.Builder.CreateError(s.Wool.Wrapf(err, "synchronize generated service"))
	}

	if err := s.CreateEndpoints(ctx); err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create endpoints")
	}

	return s.Base.Builder.CreateResponse(ctx, s.GoGrpc.Settings)
}

// syncResponseError converts the Builder protocol's in-band lifecycle status
// into a Go error for internal composition. Builder RPC methods intentionally
// encode domain failures in their response and return a nil transport error;
// checking only the second return value would let Create report success after
// generation failed.
func syncResponseError(response *builderv0.SyncResponse) error {
	if response == nil || response.GetState() == nil {
		return fmt.Errorf("sync returned no status")
	}
	state := response.GetState()
	if state.GetState() == builderv0.SyncStatus_SUCCESS {
		return nil
	}
	message := strings.TrimSpace(state.GetMessage())
	if message == "" {
		message = state.GetState().String()
	}
	return fmt.Errorf("%s", message)
}

func (s *Builder) populateSettingsFromAnswers() error {
	var err error
	if s.GoGrpc.Settings.HotReload, err = communicate.Confirm(s.answers, HotReload); err != nil {
		return err
	}
	if s.GoGrpc.Settings.DebugSymbols, err = communicate.Confirm(s.answers, DebugSymbols); err != nil {
		return err
	}
	if s.GoGrpc.Settings.RaceConditionDetectionRun, err = communicate.Confirm(s.answers, RaceConditionDetectionRun); err != nil {
		return err
	}
	if s.GoGrpc.Settings.RestEndpoint, err = communicate.Confirm(s.answers, RestEndpointSetting); err != nil {
		return err
	}
	if s.GoGrpc.Settings.ConnectEndpoint, err = communicate.Confirm(s.answers, ConnectEndpointSetting); err != nil {
		return err
	}
	return nil
}

func (s *Builder) populateSettingsFromDefaults() error {
	opts := s.Options()
	var err error
	if s.GoGrpc.Settings.HotReload, err = communicate.GetDefaultConfirm(opts, HotReload); err != nil {
		return err
	}
	if s.GoGrpc.Settings.DebugSymbols, err = communicate.GetDefaultConfirm(opts, DebugSymbols); err != nil {
		return err
	}
	if s.GoGrpc.Settings.RaceConditionDetectionRun, err = communicate.GetDefaultConfirm(opts, RaceConditionDetectionRun); err != nil {
		return err
	}
	if s.GoGrpc.Settings.RestEndpoint, err = communicate.GetDefaultConfirm(opts, RestEndpointSetting); err != nil {
		return err
	}
	if s.GoGrpc.Settings.ConnectEndpoint, err = communicate.GetDefaultConfirm(opts, ConnectEndpointSetting); err != nil {
		return err
	}
	return nil
}

// Communicate collects answers for the five Options questions.
func (s *Builder) Communicate(stream builderv0.Builder_CommunicateServer) error {
	asker := communicate.NewQuestionAsker(stream)
	answers, err := asker.RunSequence(s.Options())
	if err != nil {
		return err
	}
	s.answers = answers
	return nil
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
