package main

import (
	"context"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/configurations/standards"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/runners"
	"github.com/codefly-dev/core/shared"
	"strings"

	"github.com/codefly-dev/core/wool"

	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"

	"github.com/codefly-dev/core/agents/helpers/code"
	golanghelpers "github.com/codefly-dev/core/agents/helpers/go"
	protohelpers "github.com/codefly-dev/core/agents/helpers/proto"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
)

type Runtime struct {
	*Service

	// internal
	protohelper *protohelpers.Proto
	runner      *golanghelpers.Runner

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

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	requirements.Localize(s.Location)

	s.Environment = req.Environment

	s.EnvironmentVariables = s.LoadEnvironmentVariables(req.Environment)

	if s.Settings.Watch {
		s.Wool.Debug("setting up code watcher")
		// Add proto and swagger
		dependencies := requirements.Clone()
		dependencies.AddDependencies(
			builders.NewDependency("proto").WithPathSelect(shared.NewSelect("*.proto")),
			builders.NewDependency("proto/swagger").WithPathSelect(shared.NewSelect("*.swagger.json")),
		)
		conf := services.NewWatchConfiguration(dependencies)
		err = s.SetupWatcher(ctx, conf, s.EventHandler)
		if err != nil {
			s.Wool.Warn("error in watcher", wool.ErrField(err))
		}
	}

	err = s.LoadEndpoints(ctx, configurations.IsLocal(s.Environment))
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	s.EnvironmentVariables = configurations.NewEnvironmentVariableManager()

	return s.Base.Runtime.LoadResponse()
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.NetworkMappings = req.NetworkMappings

	net, err := configurations.GetMappingInstanceFor(s.NetworkMappings, standards.GRPC)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.Port = net.Port

	s.LogForward("gRPC will run on: %s", net.Address)

	for _, providerInfo := range req.ProviderInfos {
		envs := configurations.ProviderInformationAsEnvironmentVariables(providerInfo)
		s.EnvironmentVariables.Add(envs...)
	}

	s.protohelper, err = protohelpers.NewProto(ctx, s.Location)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	err = s.protohelper.Generate(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	runner, err := golanghelpers.NewRunner(ctx, s.Location)
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

	s.runner.WithDebug(s.Settings.Debug).WithRaceConditionDetection(s.Settings.WithRaceConditionDetectionRun).WithRequirements(requirements)
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

	return s.Runtime.InitResponse()
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

	// Be nice and wait for Port to be free
	s.Wool.Debug("waiting for port to be free")
	err = runners.WaitForPortUnbound(ctx, s.Port)
	if err != nil {
		return s.Runtime.StopError(err)
	}
	s.Wool.Debug("port is free", wool.Field("port", s.Port))

	err = s.Base.Stop()
	if err != nil {
		return s.Runtime.StopError(err)
	}
	return s.Runtime.StopResponse()
}

func (s *Runtime) Communicate(ctx context.Context, req *agentv0.Engage) (*agentv0.InformationRequest, error) {
	return s.Base.Communicate(ctx, req)
}

/* Details

 */

func (s *Runtime) EventHandler(event code.Change) error {
	// if changes to ".swagger.json":
	if strings.HasSuffix(event.Path, ".proto") {
		s.Wool.Debug("proto change detected")
		// We only re-start when ready
		//s.protohelper.Valid()
		s.Runtime.DesiredLoad()
		return nil
	}
	s.Wool.Debug("detected change requiring re-build", wool.Field("path", event.Path))
	s.Runtime.DesiredInit()
	return nil
}
