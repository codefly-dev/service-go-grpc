package main

import (
	"context"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	"github.com/codefly-dev/core/companions/proto"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/configurations/standards"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/shared"
	"strings"

	"github.com/codefly-dev/core/wool"

	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"

	"github.com/codefly-dev/core/agents/helpers/code"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
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
	runner      runners.Proc
	Environment *basev0.Environment
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("loading base")
	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Base.Runtime.LoadErrorWithDetails(err, "loading base")
	}

	s.Runtime.Scope = req.Scope

	s.LogForward("running in %s mode", s.Runtime.Scope)

	s.Environment = req.Environment

	s.EnvironmentVariables.SetEnvironment(s.Environment)
	s.EnvironmentVariables.SetNetworkScope(s.Runtime.Scope)

	s.sourceLocation, err = s.LocalDirCreate(ctx, "src")
	if err != nil {
		return s.Base.Runtime.LoadErrorWithDetails(err, "creating source location")
	}

	s.cacheLocation, err = s.LocalDirCreate(ctx, ".cache")
	if err != nil {
		return s.Base.Runtime.LoadErrorWithDetails(err, "creating cache location")
	}

	if s.Settings.Watch {
		s.Wool.Debug("setting up code watcher")
		// Add proto and swagger
		dependencies := requirements.Clone()
		dependencies.AddDependencies(
			builders.NewDependency("proto").WithPathSelect(shared.NewSelect("*.proto")),
		)
		dependencies.Localize(s.Location)
		conf := services.NewWatchConfiguration(dependencies)
		err = s.SetupWatcher(ctx, conf, s.EventHandler)
		if err != nil {
			s.Wool.Warn("error in watcher", wool.ErrField(err))
		}
	}

	s.buf, err = proto.NewBuf(ctx, s.Location)
	if err != nil {
		return s.Runtime.LoadError(err)
	}

	s.buf.WithCache(s.cacheLocation)
	if s.Watcher != nil {
		s.Watcher.Pause()
	}

	if !shared.FileExists(s.Local(standards.OpenAPIPath)) {
		err = s.buf.Generate(ctx)
		if err != nil {
			return s.Runtime.LoadError(err)
		}
	}

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Base.Runtime.LoadErrorWithDetails(err, "loading endpoints")
	}

	s.grpcEndpoint, err = configurations.FindGRPCEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Base.Runtime.LoadErrorWithDetails(err, "finding grpc endpoint")
	}

	if s.Settings.WithRestEndpoint {
		s.restEndpoint, err = configurations.FindRestEndpoint(ctx, s.Endpoints)
		if err != nil {
			return s.Base.Runtime.LoadErrorWithDetails(err, "finding rest endpoint")
		}
	}

	return s.Base.Runtime.LoadResponse()
}

func (s *Runtime) CreateRunnerEnvironment(ctx context.Context) error {
	s.Wool.Debug("creating runner environment in", wool.DirField(s.sourceLocation))
	if s.Runtime.Container() {
		s.Wool.Debug("running in container")

		dockerEnv, err := golanghelpers.NewDockerGoRunner(ctx, runtimeImage, s.sourceLocation, s.UniqueWithProject())
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create docker runner")
		}
		err = dockerEnv.Clear(ctx)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot clear the docker environment")
		}
		// Need to bind the ports
		instance, err := configurations.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.grpcEndpoint, s.Runtime.Scope)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot find network instance")
		}
		dockerEnv.WithPort(ctx, instance.Port)
		if s.WithRestEndpoint {
			restInstance, err := configurations.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.restEndpoint, s.Runtime.Scope)
			if err != nil {
				return s.Wool.Wrapf(err, "cannot find network instance")
			}
			dockerEnv.WithPort(ctx, restInstance.Port)
		}
		// Mount the service.codefly.yaml
		dockerEnv.WithFile(s.Local("service.codefly.yaml"), "/service.codefly.yaml")
		s.runnerEnvironment = dockerEnv
	} else {
		s.Wool.Debug("running locally")

		localEnv, err := golanghelpers.NewLocalGoRunner(ctx, s.sourceLocation)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create local runner")
		}
		s.runnerEnvironment = localEnv
	}

	s.runnerEnvironment.WithLocalCacheDir(s.cacheLocation)

	s.runnerEnvironment.WithDebugSymbol(s.Settings.WithDebugSymbols)
	s.runnerEnvironment.WithRaceConditionDetection(s.Settings.WithRaceConditionDetectionRun)

	s.runnerEnvironment.WithEnvironmentVariables(s.EnvironmentVariables.All()...)
	return nil
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)

	s.NetworkMappings = req.ProposedNetworkMappings

	// Filter configurations for the scope
	confs := configurations.FilterConfigurations(req.DependenciesConfigurations, s.Runtime.Scope)
	err := s.EnvironmentVariables.AddConfigurations(confs...)

	// Networking
	net, err := s.Runtime.NetworkInstance(ctx, s.NetworkMappings, s.grpcEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.LogForward("gPRC will run on localhost:%d", net.Port)

	// Only add gRPC for now
	nm, err := configurations.FindNetworkMapping(ctx, s.NetworkMappings, s.grpcEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	err = s.EnvironmentVariables.AddEndpoints(ctx, []*basev0.NetworkMapping{nm}, s.Runtime.Scope)

	if s.WithRestEndpoint {
		// Networking
		restNet, err := s.Runtime.NetworkInstance(ctx, s.NetworkMappings, s.restEndpoint)
		if err != nil {
			return s.Runtime.InitError(err)
		}
		restNm, err := configurations.FindNetworkMapping(ctx, s.NetworkMappings, s.restEndpoint)
		if err != nil {
			return s.Runtime.InitError(err)
		}
		err = s.EnvironmentVariables.AddEndpoints(ctx, []*basev0.NetworkMapping{restNm}, s.Runtime.Scope)

		s.LogForward("REST will run on http://localhost:%d", restNet.Port)
	}

	s.Wool.Info("Generating proto")
	err = s.buf.Generate(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if s.Watcher != nil {
		s.Watcher.Resume()
	}

	if s.runnerEnvironment == nil {
		s.Wool.Debug("creating runner")
		err = s.CreateRunnerEnvironment(ctx)
		if err != nil {
			return s.Runtime.InitError(err)
		}
	}

	// Stop before replacing the runner
	if s.runner != nil {
		err = s.runner.Stop(ctx)
		if err != nil {
			return s.Runtime.InitError(err)
		}
	}

	s.Wool.Debug("runner init started")
	s.Wool.Info("Building go binary")
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

	runningContext := s.Wool.Inject(context.Background())
	// Now we run
	proc, err := s.runnerEnvironment.NewProcess()
	if err != nil {
		return s.Runtime.StartError(err, wool.Field("in", "runner"))
	}
	s.runner = proc
	err = s.runner.Start(runningContext)
	if err != nil {
		return s.Runtime.StartError(err, wool.Field("in", "runner"))
	}

	s.Wool.Debug("runner started successfully")

	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Base.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()

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

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	//TODO implement me
	panic("implement me")
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
	s.Wool.Focus("stopping service")
	if strings.HasSuffix(event.Path, ".proto") {
		s.Wool.Debug("proto change detected")
		// Because we read endpoints in Load
		s.Runtime.DesiredLoad()
		return nil
	}
	s.Wool.Debug("detected change requiring re-build", wool.Field("path", event.Path))
	s.Runtime.DesiredInit()
	return nil
}
