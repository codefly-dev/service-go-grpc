package main

import (
	"context"
	"strings"

	corev1 "github.com/codefly-dev/cli/proto/v1/core"

	"github.com/codefly-dev/cli/pkg/plugins"
	"github.com/codefly-dev/cli/pkg/plugins/helpers/code"
	dockerhelpers "github.com/codefly-dev/cli/pkg/plugins/helpers/docker"
	golanghelpers "github.com/codefly-dev/cli/pkg/plugins/helpers/go"
	"github.com/codefly-dev/cli/pkg/plugins/network"
	"github.com/codefly-dev/cli/pkg/plugins/services"
	servicev1 "github.com/codefly-dev/cli/proto/v1/services"
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

func (p *Runtime) Init(req *servicev1.InitRequest) (*runtimev1.InitResponse, error) {
	defer p.PluginLogger.Catch()

	err := p.Base.Init(req, p.Spec)
	if err != nil {
		return nil, err
	}

	p.PluginLogger.Debugf("init %v", p.Spec)

	return &runtimev1.InitResponse{
		Version:   p.Version(),
		Endpoints: p.Endpoints,
	}, nil

}

func (p *Runtime) Configure(req *runtimev1.ConfigureRequest) (*runtimev1.ConfigureResponse, error) {
	defer p.PluginLogger.Catch()

	p.Runner = &golanghelpers.Runner{
		Dir:           p.Location,
		Args:          []string{"main.go"},
		ServiceLogger: plugins.NewServiceLogger(p.Identity.Name),
		PluginLogger:  p.PluginLogger,
		Debug:         p.Spec.Debug,
	}

	if p.Spec.Watch {
		conf := services.NewWatchConfiguration([]string{".", "adapters"}, "service.codefly.yaml")
		err := p.SetupWatcher(conf, p.EventHandler)
		if err != nil {
			return nil, p.PluginLogger.Wrapf(err, "watcher")
		}
		p.PluginLogger.Debugf("watching %v", conf)
	}
	p.PluginLogger.DebugMe("building binary")
	err := p.Runner.Init(context.Background())
	if err != nil {
		p.ServiceLogger.Info("Cannot init: %v", err)
		return &runtimev1.ConfigureResponse{Status: services.ConfigureError(err)}, nil
	}

	nets, err := p.Network()
	if err != nil {
		return nil, errors.Wrapf(err, "cannot create default endpoint")
	}

	return &runtimev1.ConfigureResponse{
		Status:          services.ConfigureSuccess(),
		NetworkMappings: nets,
	}, nil
}

func (p *Runtime) Start(req *runtimev1.StartRequest) (*runtimev1.StartResponse, error) {
	defer p.PluginLogger.Catch()

	p.PluginLogger.DebugMe("starting service")

	ctx := context.Background()

	p.PluginLogger.Info("network mapping: %v", req.NetworkMappings)

	p.Runner.Envs = network.ConvertToEnvironmentVariables(req.NetworkMappings)

	tracker, err := p.Runner.Run(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot run go")
	}

	return &runtimev1.StartResponse{
		Status:   p.StartSuccess(),
		Trackers: []*runtimev1.Tracker{tracker.Proto()},
	}, nil
}

func (p *Runtime) Information(req *runtimev1.InformationRequest) (*runtimev1.InformationResponse, error) {
	return &runtimev1.InformationResponse{Status: p.Status}, nil
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

func (p *Runtime) Sync(req *runtimev1.SyncRequest) (*runtimev1.SyncResponse, error) {
	defer p.PluginLogger.Catch()

	helper := golanghelpers.Go{Dir: p.Location}
	err := helper.ModTidy(p.PluginLogger)
	if err != nil {
		return nil, shared.Wrapf(err, "cannot tidy go.mod")
	}
	err = helper.BufGenerate(p.PluginLogger)
	if err != nil {
		return nil, shared.Wrapf(err, "cannot generate proto")
	}
	return &runtimev1.SyncResponse{}, nil
}

func (p *Runtime) Build(req *runtimev1.BuildRequest) (*runtimev1.BuildResponse, error) {
	p.PluginLogger.Debugf("building docker image")
	builder, err := dockerhelpers.NewBuilder(dockerhelpers.BuilderConfiguration{
		Root:  p.Location,
		Image: p.Identity.Name,
		Tag:   p.Configuration.Version,
	})
	if err != nil {
		return nil, p.PluginLogger.Wrapf(err, "cannot create builder")
	}
	builder.WithLogger(p.PluginLogger)
	_, err = builder.Build()
	if err != nil {
		return nil, p.PluginLogger.Wrapf(err, "cannot build image")
	}
	return &runtimev1.BuildResponse{}, nil
}

func (p *Runtime) Deploy(req *runtimev1.DeploymentRequest) (*runtimev1.DeploymentResponse, error) {
	return &runtimev1.DeploymentResponse{}, nil
}

func (p *Runtime) Communicate(req *corev1.Question) (*corev1.Answer, error) {
	panic("implement me")
}

/* Details

 */

func (p *Runtime) EventHandler(event code.Change) error {
	p.PluginLogger.DebugMe("got an event: %v", event)
	if strings.Contains(event.Path, "proto") {
		_, err := p.Sync(&runtimev1.SyncRequest{})
		if err != nil {
			p.PluginLogger.Warn("cannot sync proto: %v", err)
		}
	}
	err := p.Runner.Init(context.Background())
	if err != nil {
		p.ServiceLogger.Info("-> Detected code changes: still cannot restart: %v", err)
		return err
	}
	p.ServiceLogger.Info("-> Detected working code changes: restarting")
	p.PluginLogger.DebugMe("detected working code changes: restarting")
	p.WantRestart()
	return nil
}

func (p *Runtime) Network() ([]*runtimev1.NetworkMapping, error) {
	pm := network.NewServicePortManager(p.Identity, p.Endpoints...).WithHost("localhost").WithLogger(p.PluginLogger)
	for _, endpoint := range p.Endpoints {
		err := pm.Expose(endpoint)
		if err != nil {
			return nil, shared.Wrapf(err, "cannot expose endpoint")
		}
	}
	err := pm.Reserve()
	if err != nil {
		return nil, shared.Wrapf(err, "cannot reserve ports")
	}
	return pm.NetworkMapping()
}
