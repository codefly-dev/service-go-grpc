package main

import (
	"context"
	"strings"

	"github.com/codefly-dev/core/agents/services"
	agentv1 "github.com/codefly-dev/core/generated/go/services/agent/v1"

	"github.com/codefly-dev/core/agents/helpers/code"
	golanghelpers "github.com/codefly-dev/core/agents/helpers/go"
	"github.com/codefly-dev/core/agents/network"
	runtimev1 "github.com/codefly-dev/core/generated/go/services/runtime/v1"
	"github.com/codefly-dev/core/shared"
	"github.com/pkg/errors"
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

func (s *Runtime) Init(ctx context.Context, req *runtimev1.InitRequest) (*runtimev1.InitResponse, error) {
	defer s.Wool.Catch()

	err := s.Base.Init(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Base.RuntimeInitResponseError(err)
	}

	err = s.LoadEndpoints(ctx)
	if err != nil {
		return s.Base.RuntimeInitResponseError(err)
	}

	return s.Base.RuntimeInitResponse(s.Endpoints)
}

func (s *Runtime) Configure(ctx context.Context, req *runtimev1.ConfigureRequest) (*runtimev1.ConfigureResponse, error) {
	defer s.Wool.Catch()

	nets, err := s.Network(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot create default endpoint")
	}

	envs, err := network.ConvertToEnvironmentVariables(nets)
	if err != nil {
		return nil, s.Wrapf(err, "cannot convert network mappings to environment variables")
	}

	s.Runner = &golanghelpers.Runner{
		Dir:   s.Location,
		Args:  []string{"main.go"},
		Envs:  envs,
		Debug: s.Settings.Debug,
	}

	err = s.Runner.Init(context.Background())
	if err != nil {
		return &runtimev1.ConfigureResponse{Status: services.ConfigureError(err)}, nil
	}

	return &runtimev1.ConfigureResponse{
		Status:          services.ConfigureSuccess(),
		NetworkMappings: nets,
	}, nil
}

func (s *Runtime) Start(ctx context.Context, req *runtimev1.StartRequest) (*runtimev1.StartResponse, error) {
	defer s.Wool.Catch()

	envs, err := network.ConvertToEnvironmentVariables(req.NetworkMappings)
	if err != nil {
		return nil, s.Wrapf(err, "cannot convert network mappings to environment variables")
	}

	s.Runner.Envs = append(s.Runner.Envs, envs...)
	s.Runner.Envs = append(s.Runner.Envs, "CODEFLY_SDK__LOGLEVEL", "debug")

	if s.Settings.Watch {
		conf := services.NewWatchConfiguration([]string{".", "adapters"}, "service.codefly.yaml")
		err := s.SetupWatcher(ctx, conf, s.EventHandler)
		if err != nil {
			s.Wool.Warn("error in watcher")
		}
	}

	tracker, err := s.Runner.Run(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot run go")
	}

	return &runtimev1.StartResponse{
		Status:   services.StartSuccess(),
		Trackers: []*runtimev1.Tracker{tracker.Proto()},
	}, nil
}

func (s *Runtime) Information(ctx context.Context, req *runtimev1.InformationRequest) (*runtimev1.InformationResponse, error) {
	return &runtimev1.InformationResponse{}, nil
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev1.StopRequest) (*runtimev1.StopResponse, error) {
	defer s.Wool.Catch()

	s.Wool.Debug("stopping service")
	err := s.Runner.Kill()
	if err != nil {
		return nil, shared.Wrapf(err, "cannot kill go")
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
		s.WantRestart()
	}
	err := s.Runner.Init(context.Background())
	if err != nil {
		return err
	}
	return nil
}

func (s *Runtime) Network(ctx context.Context) ([]*runtimev1.NetworkMapping, error) {
	pm, err := network.NewServicePortManager(ctx, s.Identity, s.Endpoints...)
	if err != nil {
		return nil, shared.Wrapf(err, "cannot create default endpoint")
	}
	err = pm.Expose(s.GrpcEndpoint)
	if err != nil {
		return nil, shared.Wrapf(err, "cannot add grpc endpoint to network manager")
	}
	if s.RestEndpoint != nil {
		err = pm.Expose(s.RestEndpoint)
		if err != nil {
			return nil, shared.Wrapf(err, "cannot add rest to network manager")
		}
	}
	err = pm.Reserve()
	if err != nil {
		return nil, shared.Wrapf(err, "cannot reserve ports")
	}
	return pm.NetworkMapping()
}
