package main

import (
	"context"
	"path"
	"strings"

	"github.com/codefly-dev/core/agents/helpers/code"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"

	goruntime "github.com/codefly-dev/service-go/pkg/runtime"
)

// Runtime is the gRPC specialization of the generic Go Runtime.
//
// Embedding chain:
//
//	*goruntime.Runtime  — promotes Test, Lint, Build, Information,
//	                      and services.Base (Wool, Logger, Location,
//	                      Identity, EnvironmentVariables, NetworkMappings,
//	                      Endpoints, Watcher, Events, SetupWatcher, …)
//	                      via *goservice.Service.
//	GoGrpc *Service     — go-grpc-specific state: richer Settings and
//	                      the three protocol endpoints.
//
// Inherited methods: Test, Lint, Build, Information — the generic
// uv-equivalent implementations already do exactly what go-grpc needs.
//
// Overridden methods: Load, SetRuntimeContext, CreateRunnerEnvironment,
// Init, Start, Stop, Destroy — go-grpc adds gRPC/REST/Connect endpoint
// wiring, multi-port container binding, and agent command registration.
type Runtime struct {
	*goruntime.Runtime

	// GoGrpc is the go-grpc-layer service state. Access gRPC-specific
	// endpoints and the richer Settings via s.GoGrpc.*.
	GoGrpc *Service

	cacheLocation string
	runner        runners.Proc
	testProc      runners.Proc
}

// NewRuntime composes a go-grpc Runtime by constructing a generic
// goruntime.Runtime bound to the same Service and wiring the go-grpc
// outer.
func NewRuntime(svc *Service) *Runtime {
	return &Runtime{
		Runtime: goruntime.New(svc.Service),
		GoGrpc:  svc,
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {

	// Pass the go-grpc Settings (which inline-embeds goservice.Settings,
	// so the generic fields are populated via pointer-sharing in NewService)
	// rather than the promoted generic Settings pointer — otherwise
	// rest-endpoint / connect-endpoint are dropped during YAML unmarshal.
	err := s.Base.Load(ctx, req.Identity, s.GoGrpc.Settings)
	if err != nil {
		return s.Base.Runtime.LoadErrorf(err, "loading base")
	}

	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if req.DisableCatch {
		s.Wool.DisableCatch()
	}

	s.Base.Runtime.SetEnvironment(req.Environment)

	s.Service.SourceLocation, err = s.LocalDirCreate(ctx, "%s", s.Settings.GoSourceDir())
	if err != nil {
		return s.Base.Runtime.LoadErrorf(err, "creating source location")
	}

	s.cacheLocation, err = s.LocalDirCreate(ctx, ".cache")
	if err != nil {
		return s.Base.Runtime.LoadErrorf(err, "creating cache location")
	}

	if s.Watcher != nil {
		s.Watcher.Pause()
	}

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Base.Runtime.LoadErrorf(err, "loading endpoints")
	}

	s.GoGrpc.GrpcEndpoint, err = resources.FindGRPCEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Base.Runtime.LoadErrorf(err, "finding grpc endpoint")
	}

	if s.GoGrpc.Settings.RestEndpoint {
		s.GoGrpc.RestEndpoint, err = resources.FindRestEndpoint(ctx, s.Endpoints)
		if err != nil {
			return s.Base.Runtime.LoadErrorf(err, "finding rest endpoint")
		}
	}

	if s.GoGrpc.Settings.ConnectEndpoint {
		s.GoGrpc.ConnectEndpoint, err = resources.FindConnectEndpoint(ctx, s.Endpoints)
		if err != nil {
			return s.Base.Runtime.LoadErrorf(err, "finding connect endpoint")
		}
	}

	// Register agent commands
	s.registerCommands()

	return s.Base.Runtime.LoadResponse()
}

func (s *Runtime) SetRuntimeContext(_ context.Context, runtimeContext *basev0.RuntimeContext) error {
	s.Base.Runtime.RuntimeContext = golanghelpers.SetGoRuntimeContext(runtimeContext)
	return nil
}

func (s *Runtime) CreateRunnerEnvironment(ctx context.Context) error {
	s.Wool.Trace("creating runner environment in", wool.DirField(s.Identity.WorkspacePath))

	// Resolve the runtime image: settings override takes priority, else
	// fall back to the codefly-built default. Override rejects :latest to
	// keep builds reproducible.
	image := runtimeImage
	if override := s.GoGrpc.Settings.RuntimeImage; override != "" {
		parsed, perr := resources.ParsePinnedImage(override)
		if perr != nil {
			return s.Wool.Wrapf(perr, "invalid docker-image override in service.codefly.yaml")
		}
		s.Wool.Info("using docker-image override (not recommended)", wool.Field("image", parsed.FullName()))
		image = parsed
	}

	cfg := golanghelpers.RunnerConfig{
		RuntimeImage:   image,
		WorkspacePath:  s.Identity.WorkspacePath,
		RelativeSource: s.Identity.RelativeToWorkspace,
		UniqueName:     s.UniqueWithWorkspace(),
		CacheLocation:  s.cacheLocation,
		Settings:       &s.Settings.GoAgentSettings,
	}

	env, err := golanghelpers.CreateRunner(ctx, s.Base.Runtime.RuntimeContext, cfg)
	if err != nil {
		return err
	}

	// Bind endpoint ports for container runtime (agent-specific).
	if s.Base.Runtime.IsContainerRuntime() {
		instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.GoGrpc.GrpcEndpoint, resources.NewContainerNetworkAccess())
		if err != nil {
			return s.Wool.Wrapf(err, "cannot find grpc network instance")
		}
		env.WithPort(ctx, instance.Port)

		if s.GoGrpc.Settings.RestEndpoint {
			restInstance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.GoGrpc.RestEndpoint, resources.NewContainerNetworkAccess())
			if err != nil {
				return s.Wool.Wrapf(err, "cannot find rest network instance")
			}
			env.WithPort(ctx, restInstance.Port)
		}

		if s.GoGrpc.Settings.ConnectEndpoint {
			connectInstance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.GoGrpc.ConnectEndpoint, resources.NewContainerNetworkAccess())
			if err != nil {
				return s.Wool.Wrapf(err, "cannot find connect network instance")
			}
			env.WithPort(ctx, connectInstance.Port)
		}
	}

	allEnvs, err := s.EnvironmentVariables.All()
	if err != nil {
		return s.Wool.Wrapf(err, "cannot get environment variables")
	}
	env.WithEnvironmentVariables(ctx, allEnvs...)

	s.RunnerEnvironment = env
	// Share the underlying RunnerEnvironment with Code / Tooling / commands
	// so goimports / gofmt / go get / buf generate / go mod tidy all run in
	// the plugin's configured mode. Mirrors what the generic go runtime does.
	s.GoGrpc.Service.ActiveEnv = env.Env()
	return nil
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Base.Runtime.LogInitRequest(req)

	err := s.SetRuntimeContext(ctx, req.RuntimeContext)
	if err != nil {
		return s.Base.Runtime.InitErrorf(err, "cannot set runtime context")
	}

	s.Wool.Forwardf("starting execution environment in %s mode", s.Base.Runtime.RuntimeContext.Kind)

	s.EnvironmentVariables.SetRuntimeContext(s.Base.Runtime.RuntimeContext)

	s.NetworkMappings = req.ProposedNetworkMappings

	// Project configurations
	err = s.EnvironmentVariables.AddConfigurations(ctx, req.WorkspaceConfigurations...)
	if err != nil {
		return s.Base.Runtime.InitError(err)
	}

	// Filter resources for the scope
	confs := resources.FilterConfigurations(req.DependenciesConfigurations, s.Base.Runtime.RuntimeContext)

	s.Wool.Trace("adding configurations", wool.Field("configurations", resources.MakeManyConfigurationSummary(confs)))
	err = s.EnvironmentVariables.AddConfigurations(ctx, confs...)

	// Networking: a process is native to itself
	net, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.GoGrpc.GrpcEndpoint, resources.NewNativeNetworkAccess())
	if err != nil {
		return s.Base.Runtime.InitError(err)
	}

	s.Infof("gPRC will run on %s", net.Address)

	// Only add gRPC for now
	nm, err := resources.FindNetworkMapping(ctx, s.NetworkMappings, s.GoGrpc.GrpcEndpoint)
	if err != nil {
		return s.Base.Runtime.InitError(err)
	}
	err = s.EnvironmentVariables.AddEndpoints(ctx, []*basev0.NetworkMapping{nm}, resources.NewNativeNetworkAccess())

	if s.GoGrpc.Settings.RestEndpoint {
		nm, err = resources.FindNetworkMapping(ctx, s.NetworkMappings, s.GoGrpc.RestEndpoint)
		if err != nil {
			return s.Base.Runtime.InitError(err)
		}
		err = s.EnvironmentVariables.AddEndpoints(ctx, []*basev0.NetworkMapping{nm}, resources.NewNativeNetworkAccess())

		net, err = resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.GoGrpc.RestEndpoint, resources.NewNativeNetworkAccess())
		if err != nil {
			return s.Base.Runtime.InitError(err)
		}

		s.Infof("REST will run on %s", net.Address)
	}

	if s.GoGrpc.Settings.ConnectEndpoint {
		nm, err = resources.FindNetworkMapping(ctx, s.NetworkMappings, s.GoGrpc.ConnectEndpoint)
		if err != nil {
			return s.Base.Runtime.InitError(err)
		}
		err = s.EnvironmentVariables.AddEndpoints(ctx, []*basev0.NetworkMapping{nm}, resources.NewNativeNetworkAccess())

		net, err = resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.GoGrpc.ConnectEndpoint, resources.NewNativeNetworkAccess())
		if err != nil {
			return s.Base.Runtime.InitError(err)
		}

		s.Infof("Connect will run on %s", net.Address)
	}

	endpointAccesses := s.EnvironmentVariables.Endpoints()
	s.Wool.Trace("environment variables", wool.Field("endpoint", resources.MakeManyEndpointAccessSummary(endpointAccesses)))

	if s.Settings.HotReload {
		s.Wool.Trace("starting hot reload")
		// Add proto and swagger
		dependencies := requirements.Clone()
		dependencies.AddDependencies(
			builders.NewDependency("proto").WithPathSelect(shared.NewSelect("*.proto")),
		)
		dependencies.Localize(s.Location)
		s.Wool.Trace("setting up code watcher", wool.Field("dep", dependencies.All()))
		conf := services.NewWatchConfiguration(dependencies)
		err = s.SetupWatcher(ctx, conf, s.EventHandler)
		if err != nil {
			s.Wool.Warn("error in watcher", wool.ErrField(err))
		}
	} else {
		s.Wool.Trace("not hot-reloading")
	}

	if s.Watcher != nil {
		s.Watcher.Resume()
	}

	if s.RunnerEnvironment == nil {
		s.Wool.Trace("creating runner")
		err = s.CreateRunnerEnvironment(ctx)
		if err != nil {
			return s.Base.Runtime.InitErrorf(err, "cannot create runner environment")
		}
	}

	err = s.RunnerEnvironment.Init(ctx)
	if err != nil {
		s.Wool.Error("cannot init the go runner", wool.ErrField(err))
		return s.Base.Runtime.InitError(err)
	}

	s.Wool.Trace("runner init done")

	s.Wool.Info("successful init of runner")

	return s.Base.Runtime.InitResponse()
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Forwardf("building go binary...")

	// Stop before replacing the runner
	if s.runner != nil {
		err := s.runner.Stop(ctx)
		if err != nil {
			return s.Base.Runtime.StartError(err)
		}
	}

	err := s.RunnerEnvironment.BuildBinary(ctx)
	if err != nil {

		if !s.Settings.HotReload {
			return s.Base.Runtime.StartError(err)
		}
		s.Wool.Info("compile error, waiting for hot-reload")
		return s.Base.Runtime.StartResponse()
	}

	runningContext := s.Wool.Inject(context.Background())

	// Add DependenciesNetworkMappings
	err = s.EnvironmentVariables.AddEndpoints(ctx, req.DependenciesNetworkMappings, resources.NetworkAccessFromRuntimeContext(s.Base.Runtime.RuntimeContext))
	if err != nil {
		return s.Base.Runtime.StartError(err)
	}

	// Add Fixture
	s.Wool.Debug("setting fixture", wool.Field("fixture", req.Fixture))
	s.EnvironmentVariables.SetFixture(req.Fixture)

	// Now we run
	proc, err := s.RunnerEnvironment.Runner()
	if err != nil {
		return s.Base.Runtime.StartErrorf(err, "getting runner")
	}

	startEnvs, err := s.EnvironmentVariables.All()
	if err != nil {
		return s.Base.Runtime.StartErrorf(err, "getting environment variables")
	}
	proc.WithEnvironmentVariables(ctx, startEnvs...)
	proc.WithOutput(s.Logger)

	s.runner = proc
	err = s.runner.Start(runningContext)
	if err != nil {
		return s.Base.Runtime.StartErrorf(err, "starting runner")
	}

	// Supervise the binary: if it exits non-zero (e.g. mind os.Exit(1) on
	// missing API key), mark the runner failed so the codefly CLI's Follow
	// loop sees it via StartStatus and tears the whole tree down. Without
	// this goroutine the plugin happily idles while its child is dead.
	go func(p runners.Proc) {
		err := p.Wait(runningContext)
		if runningContext.Err() != nil {
			// Context cancelled from our side (Stop) — clean exit, no signal.
			return
		}
		// err may be nil if the binary exited cleanly without us asking —
		// still unexpected, but don't nil-deref when reporting it.
		if err != nil {
			s.Wool.Error("user binary exited unexpectedly", wool.ErrField(err))
		} else {
			s.Wool.Error("user binary exited unexpectedly (clean exit, context not cancelled)")
		}
		s.Base.Runtime.MarkRunnerExited(err)
	}(proc)

	s.Wool.Forwardf("service started and running")

	return s.Base.Runtime.StartResponse()
}

// Build, Test, Lint, Information are INHERITED from *goruntime.Runtime.
// The generic implementations shell out to go build / go test -json -cover
// / go vet with the same RunnerEnvironment and SourceLocation this layer
// sets up.

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()

	ctx = s.Wool.Inject(ctx)

	s.Wool.Trace("stopping service")
	if s.testProc != nil {
		s.Wool.Trace("stopping test process")
		_ = s.testProc.Stop(ctx)
		s.testProc = nil
	}
	if s.runner != nil {
		s.Wool.Trace("stopping runner")
		err := s.runner.Stop(ctx)
		if err != nil {
			return s.Base.Runtime.StopError(err)
		}
		s.Wool.Trace("runner stopped")
	}

	// Stop the file watcher to prevent CPU spin on orphaned processes
	if s.Watcher != nil {
		s.Watcher.Pause()
	}
	// Close events channel to unblock the handler goroutine
	if s.Events != nil {
		close(s.Events)
		s.Events = nil
	}

	s.Wool.Trace("base stopped")
	return s.Base.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Trace("destroying service")
	err := golanghelpers.DestroyGoRuntime(ctx, s.Base.Runtime.RuntimeContext, runtimeImage,
		s.cacheLocation, s.Identity.WorkspacePath,
		path.Join(s.Identity.RelativeToWorkspace, s.Settings.GoSourceDir()),
		s.UniqueWithWorkspace())
	if err != nil {
		return s.Base.Runtime.DestroyError(err)
	}
	return s.Base.Runtime.DestroyResponse()
}

/* Details

 */

func (s *Runtime) EventHandler(event code.Change) error {
	// ignore changes to ".swagger.json":
	if strings.HasSuffix(event.Path, ".swagger.json") {
		return nil
	}
	s.Wool.Trace("stopping service for rebuild")
	if strings.HasSuffix(event.Path, ".proto") {
		s.Wool.Trace("proto change detected")
		// Because we read endpoints in Load
		s.Base.Runtime.DesiredLoad()
		return nil
	}
	s.Wool.Info("detected change requiring re-build", wool.Field("path", event.Path))
	s.Base.Runtime.DesiredStart()
	return nil
}
