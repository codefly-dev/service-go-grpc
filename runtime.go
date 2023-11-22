package main

import (
	"context"
	"strings"

	"github.com/codefly-dev/cli/pkg/plugins/helpers/code"
	golanghelpers "github.com/codefly-dev/cli/pkg/plugins/helpers/go"
	"github.com/codefly-dev/cli/pkg/plugins/network"
	"github.com/codefly-dev/cli/pkg/plugins/services"
	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	v1 "github.com/codefly-dev/cli/proto/v1/services"
	runtimev1 "github.com/codefly-dev/cli/proto/v1/services/runtime"
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

func (p *Runtime) Init(req *v1.InitRequest) (*runtimev1.InitResponse, error) {
	defer p.PluginLogger.Catch()

	err := p.Base.Init(req, p.Settings)
	if err != nil {
		return p.Base.RuntimeInitResponseError(err)
	}

	err = p.LoadEndpoints()
	if err != nil {
		return p.Base.RuntimeInitResponseError(err)
	}

	return p.Base.RuntimeInitResponse(p.Endpoints)
}

func (p *Runtime) Configure(req *runtimev1.ConfigureRequest) (*runtimev1.ConfigureResponse, error) {
	defer p.PluginLogger.Catch()

	nets, err := p.Network()
	if err != nil {
		return nil, errors.Wrapf(err, "cannot create default endpoint")
	}

	envs, err := network.ConvertToEnvironmentVariables(nets)
	if err != nil {
		return nil, p.Wrapf(err, "cannot convert network mappings to environment variables")
	}

	p.Runner = &golanghelpers.Runner{
		Dir:           p.Location,
		Args:          []string{"main.go"},
		ServiceLogger: p.ServiceLogger,
		PluginLogger:  p.PluginLogger,
		Envs:          envs,
		Debug:         p.Settings.Debug,
	}

	p.ServiceLogger.Info("watching code changes")

	err = p.Runner.Init(context.Background())
	if err != nil {
		p.ServiceLogger.Info("-> Cannot init: %v", err)
		return &runtimev1.ConfigureResponse{Status: services.ConfigureError(err)}, nil
	}

	return &runtimev1.ConfigureResponse{
		Status:          services.ConfigureSuccess(),
		NetworkMappings: nets,
	}, nil
}

func (p *Runtime) Start(req *runtimev1.StartRequest) (*runtimev1.StartResponse, error) {
	defer p.PluginLogger.Catch()

	ctx := context.Background()

	p.PluginLogger.Debugf("network mapping: %v", req.NetworkMappings)

	envs, err := network.ConvertToEnvironmentVariables(req.NetworkMappings)
	if err != nil {
		return nil, p.Wrapf(err, "cannot convert network mappings to environment variables")
	}
	p.Runner.Envs = append(p.Runner.Envs, envs...)
	p.Runner.Envs = append(p.Runner.Envs, "CODEFLY_SDK__LOGLEVEL", "debug")

	if p.Settings.Watch {
		conf := services.NewWatchConfiguration([]string{".", "adapters"}, "service.codefly.yaml")
		err := p.SetupWatcher(conf, p.EventHandler)
		if err != nil {
			p.PluginLogger.Warn("error in watcher")
		}
	}

	tracker, err := p.Runner.Run(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot run go")
	}

	return &runtimev1.StartResponse{
		Status:   services.StartSuccess(),
		Trackers: []*runtimev1.Tracker{tracker.Proto()},
	}, nil
}

func (p *Runtime) Information(req *runtimev1.InformationRequest) (*runtimev1.InformationResponse, error) {
	return &runtimev1.InformationResponse{}, nil
}

func (p *Runtime) Stop(req *runtimev1.StopRequest) (*runtimev1.StopResponse, error) {
	defer p.PluginLogger.Catch()

	p.PluginLogger.Debugf("stopping service")
	err := p.Runner.Kill()
	if err != nil {
		return nil, shared.Wrapf(err, "cannot kill go")
	}

	err = p.Base.Stop()
	if err != nil {
		return nil, err
	}
	return &runtimev1.StopResponse{}, nil
}

func (p *Runtime) Communicate(req *corev1.Engage) (*corev1.InformationRequest, error) {
	return p.Base.Communicate(req)
}

/* Details

 */

func (p *Runtime) EventHandler(event code.Change) error {
	p.PluginLogger.Debugf("got an event: %v", event)
	if strings.Contains(event.Path, "proto") {
		p.WantSync()
	} else {
		p.WantRestart()
	}
	err := p.Runner.Init(context.Background())
	if err != nil {
		p.ServiceLogger.Info("Detected code changes: still cannot restart: %v", err)
		return err
	}
	p.ServiceLogger.Info("Detected code changes: restarting")
	return nil
}

func (p *Runtime) Network() ([]*runtimev1.NetworkMapping, error) {
	pm, err := network.NewServicePortManager(p.Context(), p.Identity, p.Endpoints...)
	if err != nil {
		return nil, shared.Wrapf(err, "cannot create default endpoint")
	}
	err = pm.Expose(p.GrpcEndpoint)
	if err != nil {
		return nil, shared.Wrapf(err, "cannot add grpc endpoint to network manager")
	}
	if p.RestEndpoint != nil {
		err = pm.Expose(p.RestEndpoint)
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
