package main

import (
	"context"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/shared"
	"path"
	"strings"

	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/agents/helpers/code"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
)

type Runtime struct {
	services.RuntimeServer

	*Service

	runnerEnvironment *golanghelpers.GoRunnerEnvironment

	// cache
	cacheLocation string

	// go runner
	runner   runners.Proc
	testProc runners.Proc
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "loading base")
	}

	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if req.DisableCatch {
		s.Wool.DisableCatch()
	}

	s.Runtime.SetEnvironment(req.Environment)

	s.sourceLocation, err = s.LocalDirCreate(ctx, "%s", s.Settings.GoSourceDir())
	if err != nil {
		return s.Runtime.LoadErrorf(err, "creating source location")
	}

	s.cacheLocation, err = s.LocalDirCreate(ctx, ".cache")
	if err != nil {
		return s.Runtime.LoadErrorf(err, "creating cache location")
	}

	if s.Watcher != nil {
		s.Watcher.Pause()
	}

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "loading endpoints")
	}

	s.GrpcEndpoint, err = resources.FindGRPCEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "finding grpc endpoint")
	}

	if s.Settings.RestEndpoint {
		s.RestEndpoint, err = resources.FindRestEndpoint(ctx, s.Endpoints)
		if err != nil {
			return s.Runtime.LoadErrorf(err, "finding rest endpoint")
		}
	}

	// Register agent commands
	s.registerCommands()

	return s.Runtime.LoadResponse()
}

func (s *Runtime) SetRuntimeContext(_ context.Context, runtimeContext *basev0.RuntimeContext) error {
	s.Runtime.RuntimeContext = golanghelpers.SetGoRuntimeContext(runtimeContext)
	return nil
}

func (s *Runtime) CreateRunnerEnvironment(ctx context.Context) error {
	s.Wool.Trace("creating runner environment in", wool.DirField(s.Identity.WorkspacePath))

	cfg := golanghelpers.RunnerConfig{
		RuntimeImage:   runtimeImage,
		WorkspacePath:  s.Identity.WorkspacePath,
		RelativeSource: s.Identity.RelativeToWorkspace,
		UniqueName:     s.UniqueWithWorkspace(),
		CacheLocation:  s.cacheLocation,
		Settings:       &s.Settings.GoAgentSettings,
	}

	env, err := golanghelpers.CreateRunner(ctx, s.Runtime.RuntimeContext, cfg)
	if err != nil {
		return err
	}

	// Bind endpoint ports for container runtime (agent-specific).
	if s.Runtime.IsContainerRuntime() {
		instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.GrpcEndpoint, resources.NewContainerNetworkAccess())
		if err != nil {
			return s.Wool.Wrapf(err, "cannot find grpc network instance")
		}
		env.WithPort(ctx, instance.Port)

		if s.Settings.RestEndpoint {
			restInstance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.RestEndpoint, resources.NewContainerNetworkAccess())
			if err != nil {
				return s.Wool.Wrapf(err, "cannot find rest network instance")
			}
			env.WithPort(ctx, restInstance.Port)
		}
	}

	allEnvs, err := s.EnvironmentVariables.All()
	if err != nil {
		return s.Wool.Wrapf(err, "cannot get environment variables")
	}
	env.WithEnvironmentVariables(ctx, allEnvs...)

	s.runnerEnvironment = env
	return nil
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)

	err := s.SetRuntimeContext(ctx, req.RuntimeContext)
	if err != nil {
		return s.Runtime.InitErrorf(err, "cannot set runtime context")
	}

	s.Wool.Forwardf("starting execution environment in %s mode", s.Runtime.RuntimeContext.Kind)

	s.EnvironmentVariables.SetRuntimeContext(s.Runtime.RuntimeContext)

	s.NetworkMappings = req.ProposedNetworkMappings

	// Project configurations
	err = s.EnvironmentVariables.AddConfigurations(ctx, req.WorkspaceConfigurations...)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	// Filter resources for the scope
	confs := resources.FilterConfigurations(req.DependenciesConfigurations, s.Runtime.RuntimeContext)

	s.Wool.Trace("adding configurations", wool.Field("configurations", resources.MakeManyConfigurationSummary(confs)))
	err = s.EnvironmentVariables.AddConfigurations(ctx, confs...)

	// Networking: a process is native to itself
	net, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.GrpcEndpoint, resources.NewNativeNetworkAccess())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.Infof("gPRC will run on %s", net.Address)

	// Only add gRPC for now
	nm, err := resources.FindNetworkMapping(ctx, s.NetworkMappings, s.GrpcEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	err = s.EnvironmentVariables.AddEndpoints(ctx, []*basev0.NetworkMapping{nm}, resources.NewNativeNetworkAccess())

	if s.Settings.RestEndpoint {
		nm, err = resources.FindNetworkMapping(ctx, s.NetworkMappings, s.RestEndpoint)
		if err != nil {
			return s.Runtime.InitError(err)
		}
		err = s.EnvironmentVariables.AddEndpoints(ctx, []*basev0.NetworkMapping{nm}, resources.NewNativeNetworkAccess())

		net, err = resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.RestEndpoint, resources.NewNativeNetworkAccess())
		if err != nil {
			return s.Runtime.InitError(err)
		}

		s.Infof("REST will run on %s", net.Address)
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

	if s.runnerEnvironment == nil {
		s.Wool.Trace("creating runner")
		err = s.CreateRunnerEnvironment(ctx)
		if err != nil {
			return s.Runtime.InitErrorf(err, "cannot create runner environment")
		}
	}

	err = s.runnerEnvironment.Init(ctx)
	if err != nil {
		s.Wool.Error("cannot init the go runner", wool.ErrField(err))
		return s.Runtime.InitError(err)
	}

	s.Wool.Trace("runner init done")

	s.Wool.Info("successful init of runner")

	return s.Runtime.InitResponse()
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Forwardf("building go binary...")

	// Stop before replacing the runner
	if s.runner != nil {
		err := s.runner.Stop(ctx)
		if err != nil {
			return s.Runtime.StartError(err)
		}
	}

	err := s.runnerEnvironment.BuildBinary(ctx)
	if err != nil {

		if !s.Settings.HotReload {
			return s.Runtime.StartError(err)
		}
		s.Wool.Info("compile error, waiting for hot-reload")
		return s.Runtime.StartResponse()
	}

	runningContext := s.Wool.Inject(context.Background())

	// Add DependenciesNetworkMappings
	err = s.EnvironmentVariables.AddEndpoints(ctx, req.DependenciesNetworkMappings, resources.NetworkAccessFromRuntimeContext(s.Runtime.RuntimeContext))
	if err != nil {
		return s.Runtime.StartError(err)
	}

	// Add Fixture
	s.Wool.Debug("setting fixture", wool.Field("fixture", req.Fixture))
	s.EnvironmentVariables.SetFixture(req.Fixture)

	// Now we run
	proc, err := s.runnerEnvironment.Runner()
	if err != nil {
		return s.Runtime.StartErrorf(err, "getting runner")
	}

	startEnvs, err := s.EnvironmentVariables.All()
	if err != nil {
		return s.Runtime.StartErrorf(err, "getting environment variables")
	}
	proc.WithEnvironmentVariables(ctx, startEnvs...)
	proc.WithOutput(s.Logger)

	s.runner = proc
	err = s.runner.Start(runningContext)
	if err != nil {
		return s.Runtime.StartErrorf(err, "starting runner")
	}

	s.Wool.Forwardf("service started and running")

	return s.Runtime.StartResponse()
}

func (s *Runtime) Build(ctx context.Context, req *runtimev0.BuildRequest) (*runtimev0.BuildResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Infof("running go build")

	envs, err := s.EnvironmentVariables.All()
	if err != nil {
		return s.Runtime.BuildErrorf(err, "getting environment variables")
	}

	opts := golanghelpers.BuildOptions{Target: req.Target}
	output, runErr := golanghelpers.RunGoBuild(ctx, s.runnerEnvironment, s.sourceLocation, envs, opts)
	if runErr != nil {
		return s.Runtime.BuildErrorf(runErr, "build failed")
	}
	return s.Runtime.BuildResponse(output)
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Infof("running go tests")

	testEnvs, err := s.EnvironmentVariables.All()
	if err != nil {
		return s.Runtime.TestErrorf(err, "getting environment variables")
	}

	opts := golanghelpers.TestOptions{
		Target:  req.Target,
		Verbose: req.Verbose,
		Race:    req.Race,
		Timeout: req.Timeout,
	}
	summary, runErr := golanghelpers.RunGoTests(ctx, s.runnerEnvironment, s.sourceLocation, testEnvs, opts)

	// Forward summary and failures to the logger for the TUI
	s.Wool.Forwardf("Tests: %s", summary.SummaryLine())
	for _, f := range summary.Failures {
		s.Wool.Forwardf("%s", f)
	}

	return s.Runtime.TestResponseWithResults(summary.Run, summary.Passed, summary.Failed, summary.Skipped, summary.Coverage, summary.Failures, runErr)
}

func (s *Runtime) Lint(ctx context.Context, req *runtimev0.LintRequest) (*runtimev0.LintResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Infof("running go vet")

	envs, err := s.EnvironmentVariables.All()
	if err != nil {
		return s.Runtime.LintErrorf(err, "getting environment variables")
	}

	opts := golanghelpers.LintOptions{Target: req.Target}
	output, runErr := golanghelpers.RunGoLint(ctx, s.runnerEnvironment, s.sourceLocation, envs, opts)
	if runErr != nil {
		return s.Runtime.LintErrorf(runErr, "lint failed")
	}
	return s.Runtime.LintResponse(output)
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

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
			return s.Runtime.StopError(err)
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
	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Trace("destroying service")
	err := golanghelpers.DestroyGoRuntime(ctx, s.Runtime.RuntimeContext, runtimeImage,
		s.cacheLocation, s.Identity.WorkspacePath,
		path.Join(s.Identity.RelativeToWorkspace, s.Settings.GoSourceDir()),
		s.UniqueWithWorkspace())
	if err != nil {
		return s.Runtime.DestroyError(err)
	}
	return s.Runtime.DestroyResponse()
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
		s.Runtime.DesiredLoad()
		return nil
	}
	s.Wool.Info("detected change requiring re-build", wool.Field("path", event.Path))
	s.Runtime.DesiredStart()
	return nil
}
