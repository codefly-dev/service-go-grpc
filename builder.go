package main

import (
	"context"
	"embed"

	"github.com/codefly-dev/core/agents/communicate"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/agents/services/audit"
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

	buf           *proto.Buf
	cacheLocation string
	answers       map[string]*agentv0.Answer
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
	s.cacheLocation = s.Local(".cache")

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

// Sync regenerates local proto code via buf and generates gRPC client
// stubs for every declared dependency that exposes a gRPC endpoint.
func (s *Builder) Sync(ctx context.Context, _ *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if s.buf == nil {
		var err error
		s.buf, err = proto.NewBuf(ctx, s.Location)
		if err != nil {
			return s.Base.Builder.SyncError(err)
		}
		s.buf.WithCache(s.cacheLocation)
	}

	if err := s.buf.Generate(ctx); err != nil {
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
		if err := proto.GenerateGRPC(ctx, languages.GO, s.Local("code/external/%s", dep.Unique()), dep.Unique(), ep); err != nil {
			return s.Base.Builder.SyncError(err)
		}
	}
	return s.Base.Builder.SyncResponse()
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

// Audit scans the Go module for vulnerabilities (govulncheck, callgraph-
// aware) and optionally reports outdated deps (go list -m -u). Runs at
// the Go source root (s.Location + Settings.GoSourceDir()).
func (s *Builder) Audit(ctx context.Context, req *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	dir := s.Local("%s", s.GoGrpc.Settings.GoSourceDir())
	res, err := audit.Golang(ctx, dir, req.IncludeOutdated)
	if err != nil {
		return s.Base.Builder.AuditError(err)
	}
	return s.Base.Builder.AuditResponse(res.Findings, res.Outdated, res.Tool, res.Language)
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
	Envs []string
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

	create := CreateConfiguration{Information: s.Information, Envs: []string{}}
	ignore := shared.NewIgnore("go.work*", "service.generation.codefly.yaml")
	override := shared.OverrideException(shared.NewIgnore("*.proto"))

	if err := s.Templates(ctx, create, services.WithFactory(factoryFS).WithPathSelect(ignore).WithOverride(override)); err != nil {
		return s.Base.Builder.CreateError(err)
	}

	if err := s.CreateEndpoints(ctx); err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create endpoints")
	}

	return s.Base.Builder.CreateResponse(ctx, s.GoGrpc.Settings)
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
