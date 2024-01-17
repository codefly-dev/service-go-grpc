package main

import (
	"context"

	"github.com/codefly-dev/core/configurations"

	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/agents/services"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"

	"github.com/codefly-dev/core/agents/helpers/code"
	golanghelpers "github.com/codefly-dev/core/agents/helpers/go"
	protohelpers "github.com/codefly-dev/core/agents/helpers/proto"
	"github.com/codefly-dev/core/agents/network"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
)

type Runtime struct {
	*Service

	// internal
	protohelper *protohelpers.Proto
	runner      *golanghelpers.Runner

	EnvironmentVariables *configurations.EnvironmentVariableManager
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

	err = s.LoadEndpoints(ctx)
	if err != nil {
		return s.Base.Runtime.LoadError(err)
	}

	s.EnvironmentVariables = configurations.NewEnvironmentVariableManager()

	return s.Base.Runtime.LoadResponse()
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	var err error
	s.NetworkMappings, err = s.Network(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	for _, providerInfo := range req.ProviderInfos {
		envs := configurations.ProviderInformationAsEnvironmentVariables(providerInfo)
		s.EnvironmentVariables.Add(envs...)
	}

	s.runner = &golanghelpers.Runner{
		Dir:   s.Location,
		Args:  []string{"main.go"},
		Debug: s.Settings.Debug,
	}
	s.protohelper, err = protohelpers.NewProto(ctx, s.Local("proto"))
	if err != nil {
		return s.Runtime.InitError(err)
	}

	err = s.protohelper.Generate(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	err = s.runner.Init(ctx)
	if err != nil {
		s.Wool.Error("cannot init the go runner", wool.ErrField(err))
		return s.Runtime.InitError(err)
	}
	s.Ready()
	s.Wool.Info("successful init of runner")

	return s.Runtime.InitResponse()
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	// Self-mapping
	envs, err := network.ConvertToEnvironmentVariables(s.NetworkMappings)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	s.EnvironmentVariables.Add(envs...)

	others, err := network.ConvertToEnvironmentVariables(req.NetworkMappings)
	if err != nil {
		return s.Runtime.StartError(err, wool.Field("in", "convert to environment variables"))
	}

	s.EnvironmentVariables.Add(others...)

	s.runner.Envs = s.EnvironmentVariables.Get()

	if s.Settings.Watch {
		conf := services.NewWatchConfiguration(requirements)
		err := s.SetupWatcher(ctx, conf, s.EventHandler)
		if err != nil {
			s.Wool.Warn("error in watcher", wool.ErrField(err))
		}
	}

	// Create a new context as the runner will be running in the background
	runningContext := context.Background()
	runningContext = s.Wool.Inject(runningContext)

	out, err := s.runner.Start(runningContext)
	if err != nil {
		return s.Runtime.StartError(err, wool.Field("in", "runner"))
	}

	go func() {
		for event := range out.Events {
			if event.Err != nil && event.Message != "" {
				s.Wool.Error("event", wool.Field("event", event))
			}
		}
	}()

	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Base.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()

	s.Wool.Debug("stopping service")
	err := s.runner.Kill(ctx)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot kill go")
	}

	err = s.Base.Stop()
	if err != nil {
		return nil, err
	}
	return &runtimev0.StopResponse{}, nil
}

func (s *Runtime) Communicate(ctx context.Context, req *agentv0.Engage) (*agentv0.InformationRequest, error) {
	return s.Base.Communicate(ctx, req)
}

/* Details

 */

func (s *Runtime) EventHandler(event code.Change) error {
	s.WantRestart()
	return nil
}
