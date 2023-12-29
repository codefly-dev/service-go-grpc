package main

import (
	"context"
	"strings"

	"github.com/codefly-dev/core/runners"

	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/agents/services"
	agentv1 "github.com/codefly-dev/core/generated/go/services/agent/v1"

	"github.com/codefly-dev/core/agents/helpers/code"
	golanghelpers "github.com/codefly-dev/core/agents/helpers/go"
	"github.com/codefly-dev/core/agents/network"
	runtimev1 "github.com/codefly-dev/core/generated/go/services/runtime/v1"
)

type Runtime struct {
	*Service

	// internal
	Runner *golanghelpers.Runner
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev1.LoadRequest) (*runtimev1.LoadResponse, error) {
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

	return s.Base.Runtime.LoadResponse(s.Endpoints)
}

func (s *Runtime) Init(ctx context.Context, req *runtimev1.InitRequest) (*runtimev1.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("initialize runtime", wool.NullableField("dependency endpoints", req.DependenciesEndpoints))

	var err error
	s.NetworkMappings, err = s.Network(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if s.Settings.Watch {
		conf := services.NewWatchConfiguration([]string{".", "adapters"}, "service.codefly.yaml")
		err := s.SetupWatcher(ctx, conf, s.EventHandler)
		if err != nil {
			s.Wool.Warn("error in watcher")
		}
	}

	s.Runner = &golanghelpers.Runner{
		Dir:   s.Location,
		Args:  []string{"main.go"},
		Debug: s.Settings.Debug,
	}

	err = s.Runner.Init(ctx)
	if err != nil {
		s.Wool.Error("cannot init the go runner", wool.ErrField(err))
		return s.Runtime.InitError(err)
	}
	s.Ready()
	s.Wool.Info("successful init of runner")

	return s.Runtime.InitResponse()
}

func (s *Runtime) Start(ctx context.Context, req *runtimev1.StartRequest) (*runtimev1.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	// Self-mapping
	envs, err := network.ConvertToEnvironmentVariables(s.NetworkMappings)
	if err != nil {
		return s.Runtime.StartError(err)
	}
	s.Runner.Envs = envs

	others, err := network.ConvertToEnvironmentVariables(req.NetworkMappings)
	if err != nil {
		return s.Runtime.StartError(err, wool.Field("in", "convert to environment variables"))
	}

	s.Runner.Envs = append(s.Runner.Envs, others...)

	// TODO: put this into core
	s.Runner.Envs = append(s.Runner.Envs, "CODEFLY_SDK__LOGLEVEL", "debug")

	if s.Settings.Watch {
		conf := services.NewWatchConfiguration([]string{".", "adapters"}, "service.codefly.yaml")
		err := s.SetupWatcher(ctx, conf, s.EventHandler)
		if err != nil {
			s.Wool.Warn("error in watcher")
		}
	}

	// Create a new context as the runner will be running in the background
	runningContext := context.Background()
	runningContext = s.Wool.Inject(runningContext)

	// TODO: Helps with error handling
	out, err := s.Runner.Run(runningContext)
	if err != nil {
		return s.Runtime.StartError(err, wool.Field("in", "runner"))
	}

	go func() {
		for event := range out.Events {
			s.Wool.Error("event", wool.Field("event", event))
		}
	}()

	tracker := runners.TrackedProcess{PID: out.PID}
	s.Info("starting", wool.Field("pid", out.PID))

	return s.Runtime.StartResponse([]*runtimev1.Tracker{tracker.Proto()})
}

func (s *Runtime) Information(ctx context.Context, req *runtimev1.InformationRequest) (*runtimev1.InformationResponse, error) {
	return s.Base.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev1.StopRequest) (*runtimev1.StopResponse, error) {
	defer s.Wool.Catch()

	s.Wool.Debug("stopping service")
	err := s.Runner.Kill(ctx)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot kill go")
	}

	err = s.Base.Stop()
	if err != nil {
		return nil, err
	}
	return &runtimev1.StopResponse{}, nil
}

func (s *Runtime) Communicate(ctx context.Context, req *agentv1.Engage) (*agentv1.InformationRequest, error) {
	return s.Base.Communicate(ctx, req)
}

/* Details

 */

func (s *Runtime) EventHandler(event code.Change) error {
	if strings.Contains(event.Path, "proto") {
		s.WantSync()
	} else {
		s.Wool.Info("detected a code change")
		s.WantRestart()
	}

	return nil
}

func (s *Runtime) Network(ctx context.Context) ([]*runtimev1.NetworkMapping, error) {
	pm, err := network.NewServicePortManager(ctx, s.Identity, s.Endpoints...)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create default endpoint")
	}
	err = pm.Expose(s.GrpcEndpoint)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot add grpc endpoint to network manager")
	}
	if s.RestEndpoint != nil {
		err = pm.Expose(s.RestEndpoint)
		if err != nil {
			return nil, s.Wool.Wrapf(err, "cannot add rest to network manager")
		}
	}
	err = pm.Reserve(ctx)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot reserve ports")
	}
	return pm.NetworkMapping(ctx)
}
