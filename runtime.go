package main

import (
	"context"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	"github.com/codefly-dev/core/companions/proto"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/shared"
	"path"
	"strings"

	"github.com/codefly-dev/core/wool"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"

	"github.com/codefly-dev/core/agents/helpers/code"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
)

type Runtime struct {
	*Service

	runnerEnvironment *golanghelpers.GoRunnerEnvironment

	// cache
	cacheLocation string

	// proto
	buf *proto.Buf

	// go runner
	runner runners.Proc
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

	s.sourceLocation, err = s.LocalDirCreate(ctx, "code")
	if err != nil {
		return s.Runtime.LoadErrorf(err, "creating source location")
	}

	s.cacheLocation, err = s.LocalDirCreate(ctx, ".cache")
	if err != nil {
		return s.Runtime.LoadErrorf(err, "creating cache location")
	}

	s.buf, err = proto.NewBuf(ctx, s.Location)
	if err != nil {
		return s.Runtime.LoadError(err)
	}

	s.buf.WithCache(s.cacheLocation)

	if s.Watcher != nil {
		s.Watcher.Pause()
	}

	err = s.buf.Generate(ctx, true)
	if err != nil {
		return s.Runtime.LoadError(err)
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

	return s.Runtime.LoadResponse()
}

func (s *Runtime) SetRuntimeContext(ctx context.Context, runtimeContext *basev0.RuntimeContext) error {
	if runtimeContext.Kind == resources.RuntimeContextFree || runtimeContext.Kind == resources.RuntimeContextNative {
		if languages.HasGoRuntime(nil) {
			s.Runtime.RuntimeContext = resources.NewRuntimeContextNative()
			return nil
		}
	}
	s.Runtime.RuntimeContext = resources.NewRuntimeContextContainer()
	return nil
}

func (s *Runtime) CreateRunnerEnvironment(ctx context.Context) error {
	s.Wool.Debug("creating runner environment in", wool.DirField(s.Identity.WorkspacePath))

	if s.Runtime.IsContainerRuntime() {
		dockerEnv, err := golanghelpers.NewDockerGoRunner(ctx, runtimeImage, s.Identity.WorkspacePath,
			path.Join(s.Identity.RelativeToWorkspace, "code"), s.UniqueWithWorkspace())
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create docker runner")
		}

		// Need to bind the ports
		instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.GrpcEndpoint, resources.NewContainerNetworkAccess())
		if err != nil {
			return s.Wool.Wrapf(err, "cannot find network instance")
		}

		dockerEnv.WithPort(ctx, instance.Port)

		if s.Settings.RestEndpoint {
			restInstance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.RestEndpoint, resources.NewContainerNetworkAccess())
			if err != nil {
				return s.Wool.Wrapf(err, "cannot find network instance")
			}
			dockerEnv.WithPort(ctx, restInstance.Port)
		}
		// Mount the service.codefly.yaml
		dockerEnv.WithFile(s.Local("service.codefly.yaml"), "/service.codefly.yaml")
		s.runnerEnvironment = dockerEnv
	} else {
		localEnv, err := golanghelpers.NewNativeGoRunner(ctx, s.Identity.WorkspacePath, path.Join(s.Identity.RelativeToWorkspace, "code"))
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create local runner")
		}
		s.runnerEnvironment = localEnv
	}

	s.runnerEnvironment.WithLocalCacheDir(s.cacheLocation)

	s.runnerEnvironment.WithDebugSymbol(s.Settings.DebugSymbols)
	s.runnerEnvironment.WithRaceConditionDetection(s.Settings.RaceConditionDetectionRun)
	s.runnerEnvironment.WithEnvironmentVariables(ctx, s.EnvironmentVariables.All()...)

	s.runnerEnvironment.WithCGO(s.WithCGO)
	s.runnerEnvironment.WithWorkspace(s.WithWorkspace)

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

	// Caching included
	err = s.buf.Generate(ctx, true)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.NetworkMappings = req.ProposedNetworkMappings

	// Project configurations
	err = s.EnvironmentVariables.AddConfigurations(ctx, req.WorkspaceConfigurations...)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	// Filter resources for the scope
	confs := resources.FilterConfigurations(req.DependenciesConfigurations, s.Runtime.RuntimeContext)

	s.Wool.Debug("adding configurations", wool.Field("configurations", resources.MakeManyConfigurationSummary(confs)))
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
	s.Wool.Debug("environment variables", wool.Field("endpoint", resources.MakeManyEndpointAccessSummary(endpointAccesses)))

	if s.Settings.HotReload {
		s.Wool.Debug("starting hot reload")
		// Add proto and swagger
		dependencies := requirements.Clone()
		dependencies.AddDependencies(
			builders.NewDependency("proto").WithPathSelect(shared.NewSelect("*.proto")),
		)
		dependencies.Localize(s.Location)
		s.Wool.Debug("setting up code watcher", wool.Field("dep", dependencies.All()))
		conf := services.NewWatchConfiguration(dependencies)
		err = s.SetupWatcher(ctx, conf, s.EventHandler)
		if err != nil {
			s.Wool.Warn("error in watcher", wool.ErrField(err))
		}
	} else {
		s.Wool.Debug("not hot-reloading")
	}

	if s.Watcher != nil {
		s.Watcher.Resume()
	}

	if s.runnerEnvironment == nil {
		s.Wool.Debug("creating runner")
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

	s.Wool.Debug("runner init done")
	s.Ready()

	s.Wool.Info("successful init of runner")

	return s.Runtime.InitResponse()
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Info("Building go binary")

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
	s.EnvironmentVariables.SetFixture(req.Fixture)

	// Now we run
	proc, err := s.runnerEnvironment.Runner()
	if err != nil {
		return s.Runtime.StartErrorf(err, "getting runner")
	}

	proc.WithEnvironmentVariables(ctx, s.EnvironmentVariables.All()...)

	s.runner = proc
	err = s.runner.Start(runningContext)
	if err != nil {
		return s.Runtime.StartErrorf(err, "starting runner")
	}

	s.Wool.Debug("runner started successfully")

	return s.Runtime.StartResponse()
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.runnerEnvironment.Env().WithBinary("codefly")
	if err != nil {
		s.Wool.Warn("codefly binary not found in environment: testing with dependencies will not work")
	}

	proc, err := s.runnerEnvironment.Env().NewProcess("go", "test", "-v", "./...")
	if err != nil {
		return s.Runtime.TestErrorf(err, "cannot create test proc")
	}

	proc.WithOutput(s.Logger)
	proc.WithDir(s.sourceLocation)
	proc.WithEnvironmentVariables(ctx, s.EnvironmentVariables.All()...)

	s.Infof("running go tests")

	testingContext := s.Wool.Inject(context.Background())

	err = proc.Run(testingContext)

	if err != nil {
		return s.Runtime.TestErrorf(err, "cannot run tests successfully")
	}
	return s.Runtime.TestResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()

	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("stopping service")
	if s.runner != nil {
		s.Wool.Debug("stopping runner")
		err := s.runner.Stop(ctx)
		if err != nil {
			return s.Runtime.StopError(err)
		}
		s.Wool.Debug("runner stopped")
	}

	err := s.Base.Stop()
	if err != nil {
		return s.Runtime.StopError(err)
	}

	s.Wool.Debug("base stopped")
	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()

	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("Destroying service")

	// Remove cache
	s.Wool.Debug("removing cache")
	err := shared.EmptyDir(ctx, s.cacheLocation)
	if err != nil {
		return s.Runtime.DestroyError(err)
	}

	// Get the runner environment
	if s.Runtime.IsContainerRuntime() {
		s.Wool.Debug("running in container")

		dockerEnv, err := golanghelpers.NewDockerGoRunner(ctx, runtimeImage, s.Identity.WorkspacePath, path.Join(s.Identity.RelativeToWorkspace, "code"), s.UniqueWithWorkspace())
		if err != nil {
			return s.Runtime.DestroyError(err)
		}
		err = dockerEnv.Shutdown(ctx)
		if err != nil {
			return s.Runtime.DestroyError(err)
		}
	}
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) Communicate(ctx context.Context, req *agentv0.Engage) (*agentv0.InformationRequest, error) {
	return s.Base.Communicate(ctx, req)
}

/* Details

 */

func (s *Runtime) EventHandler(event code.Change) error {
	// ignore changes to ".swagger.json":
	if strings.HasSuffix(event.Path, ".swagger.json") {
		return nil
	}
	s.Wool.Debug("stopping service")
	if strings.HasSuffix(event.Path, ".proto") {
		s.Wool.Debug("proto change detected")
		// Because we read endpoints in Load
		s.Runtime.DesiredLoad()
		return nil
	}
	s.Wool.Info("detected change requiring re-build", wool.Field("path", event.Path))
	s.Runtime.DesiredStart()
	return nil
}
