package main

import (
	"context"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/configurations/standards"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/shared"
	"strings"

	"github.com/codefly-dev/core/wool"

	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"

	"github.com/codefly-dev/core/agents/helpers/code"
	golanghelpers "github.com/codefly-dev/core/agents/helpers/go"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
	"github.com/codefly-dev/core/generators"
)

type Runtime struct {
	*Service

	// Cache
	CacheLocation string

	// proto
	protohelper *generators.Proto

	// go runner
	SourceLocation string
	runner         *golanghelpers.Runner

	Environment          *basev0.Environment
	EnvironmentVariables *configurations.EnvironmentVariableManager

	NetworkMappings []*basev0.NetworkMapping
	Port            int
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

	s.SourceLocation, err = s.LocalDirCreate(ctx, "src")
	if err != nil {
		return s.Base.Runtime.LoadErrorWithDetails(err, "creating source location")
	}
	s.CacheLocation, err = s.LocalDirCreate(ctx, ".cache")
	if err != nil {
		return s.Base.Runtime.LoadErrorWithDetails(err, "creating cache location")
	}

	s.Environment = req.Environment

	s.EnvironmentVariables = s.LoadEnvironmentVariables(req.Environment)

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

	err = s.LoadEndpoints(ctx, configurations.IsLocal(s.Environment))
	if err != nil {
		return s.Base.Runtime.LoadErrorWithDetails(err, "loading endpoints")
	}

	s.EnvironmentVariables = configurations.NewEnvironmentVariableManager()

	return s.Base.Runtime.LoadResponse()
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.NetworkMappings = req.ProposedNetworkMappings

	net, err := configurations.GetMappingInstanceFor(s.NetworkMappings, standards.GRPC)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.Port = net.Port

	s.Info("gRPC will run on", wool.Field("address", net.Address))

	if s.WithRestEndpoint {
		net, err = configurations.GetMappingInstanceFor(s.NetworkMappings, standards.REST)
		if err != nil {
			return s.Runtime.InitError(err)
		}
		s.Info("REST will run on", wool.Field("address", net.Address))

	}

	for _, providerInfo := range req.ProviderInfos {
		envs := configurations.ProviderInformationAsEnvironmentVariables(providerInfo)
		s.EnvironmentVariables.Add(envs...)
	}

	s.protohelper, err = generators.NewProto(ctx, s.Location)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.protohelper.WithCache(s.CacheLocation)
	if s.Watcher != nil {
		s.Watcher.Pause()
	}
	err = s.protohelper.Generate(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if s.Watcher != nil {
		s.Watcher.Resume()
	}
	runner, err := golanghelpers.NewRunner(ctx, s.SourceLocation)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	// Stop before replacing the runner
	if s.runner != nil {
		err = s.runner.Stop()
		if err != nil {
			return s.Runtime.InitError(err)
		}
	}
	s.runner = runner

	s.runner.WithDebug(s.Settings.Debug)
	s.runner.WithRaceConditionDetection(s.Settings.WithRaceConditionDetectionRun)
	s.runner.WithRequirements(requirements)
	s.runner.WithCache(s.CacheLocation)
	// Output to wool
	s.runner.WithOut(s.Wool)

	s.Wool.Debug("runner init started")
	err = s.runner.Init(ctx)
	if err != nil {
		s.Wool.Error("cannot init the go runner", wool.ErrField(err))
		return s.Runtime.InitError(err)
	}
	s.Wool.Debug("runner init done")
	s.Ready()

	s.Wool.Info("successful init of runner")

	return s.Runtime.InitResponse(s.NetworkMappings)
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	// Self-mapping
	envs, err := configurations.ExtractEndpointEnvironmentVariables(ctx, s.NetworkMappings)
	if err != nil {
		return s.Runtime.StartError(err)
	}
	s.Wool.Debug("self network mapping", wool.Field("envs", envs))

	s.EnvironmentVariables.Add(envs...)

	others, err := configurations.ExtractEndpointEnvironmentVariables(ctx, req.OtherNetworkMappings)
	if err != nil {
		return s.Runtime.StartError(err, wool.Field("in", "convert to environment variables"))
	}

	s.Wool.Debug("other network mappings", wool.Field("envs", others))

	s.EnvironmentVariables.Add(others...)

	s.runner.WithEnvs(s.EnvironmentVariables.Get())

	runningContext := s.Wool.Inject(context.Background())

	s.Wool.Debug("starting runner")
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
	err := s.runner.Stop()

	if err != nil {
		return s.Runtime.StopError(err)
	}
	s.Wool.Debug("runner stopped")

	err = s.Base.Stop()
	if err != nil {
		return s.Runtime.StopError(err)
	}

	s.Wool.Debug("base stopped")
	return s.Runtime.StopResponse()
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
