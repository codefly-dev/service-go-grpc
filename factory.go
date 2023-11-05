package main

import (
	"embed"
	"fmt"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/codefly-dev/cli/pkg/plugins/communicate"
	golanghelpers "github.com/codefly-dev/cli/pkg/plugins/helpers/go"
	"github.com/codefly-dev/cli/pkg/plugins/services"
	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	v1 "github.com/codefly-dev/cli/proto/v1/services"
	factoryv1 "github.com/codefly-dev/cli/proto/v1/services/factory"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
)

type Factory struct {
	*Service

	// Communication
	create *communicate.ClientContext
}

func NewFactory() *Factory {
	return &Factory{
		Service: NewService(),
	}
}

type Proto struct {
	Package      string
	PackageAlias string
}

type CreateService struct {
	Name      string
	TitleName string
	Proto     Proto
	Go        GenerateInstructions
}

type GenerateInstructions struct {
	Package string
}

type Readme struct {
	Summary string
}

type CreateConfiguration struct {
	Name        string
	Destination string
	Namespace   string
	Domain      string
	Service     CreateService
	Plugin      configurations.Plugin
	Readme      Readme
}

func (p *Factory) NewCreateCommunicate() (*communicate.ClientContext, error) {
	client := communicate.NewClientContext(communicate.Create, p.PluginLogger)
	err := client.NewSequence(
		client.NewConfirm(&corev1.Message{Name: "watch", Message: "Code hot-reload (Recommended)?", Description: "Let codefly restart/resync your service when code changes are detected"}, true),
		client.NewConfirm(&corev1.Message{Name: "debug-build", Message: "Local debug with symbols build (Recommended)?", Description: "Debugging will be easier!"}, true),
		client.NewConfirm(&corev1.Message{Name: "rest", Message: "Transcode gRPC to REST?", Description: "Automatically get a REST endpoint from your gRPC definition"}, false),
	)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func (p *Factory) Init(req *v1.InitRequest) (*factoryv1.InitResponse, error) {
	defer p.PluginLogger.Catch()

	err := p.Base.Init(req, &p.Spec)
	if err != nil {
		return nil, err
	}

	p.create, err = p.NewCreateCommunicate()
	if err != nil {
		return nil, err
	}

	channels, err := p.WithCommunications(services.NewChannel(communicate.Create, p.create))
	if err != nil {
		return nil, err
	}

	return &factoryv1.InitResponse{
		Version:  p.Version(),
		Channels: channels,
	}, nil
}

func (p *Factory) Create(req *factoryv1.CreateRequest) (*factoryv1.CreateResponse, error) {
	defer p.PluginLogger.Catch()

	// Make sure the communication for create has been done successfully
	if !p.create.Ready() {
		return nil, p.PluginLogger.Errorf("create: communication not ready")
	}

	//p.Spec.Watch =
	p.ServiceLogger.Info("Creating service")

	create := CreateConfiguration{
		Name:      cases.Title(language.English, cases.NoLower).String(p.Identity.Name),
		Domain:    p.Identity.Domain,
		Namespace: p.Identity.Namespace,
		Readme:    Readme{Summary: p.Identity.Name},
	}

	// Templatize as usual
	err := templates.CopyAndApply(p.PluginLogger, templates.NewEmbeddedFileSystem(factory), shared.NewDir("templates/factory"),
		shared.NewDir(p.Location), create)
	if err != nil {
		return nil, p.PluginLogger.Wrapf(err, "cannot copy and apply template")
	}

	err = templates.CopyAndApply(p.PluginLogger, templates.NewEmbeddedFileSystem(builder), shared.NewDir("templates/builder"),
		shared.NewDir(p.Local("builder")), nil)
	if err != nil {
		return nil, p.PluginLogger.Wrapf(err, "cannot copy and apply template")
	}

	out, err := shared.GenerateTree(p.Location, " ")
	if err != nil {
		return nil, err
	}
	p.PluginLogger.Info("tree: %s", out)

	// Load default
	err = configurations.LoadSpec(req.Spec, &p.Spec, shared.BaseLogger(p.PluginLogger))
	if err != nil {
		return nil, err
	}

	endpoints, err := p.InitEndpoints()
	if err != nil {
		return nil, err
	}

	//	May override or check spec here
	spec, err := configurations.SerializeSpec(p.Spec)
	if err != nil {
		return nil, err
	}

	helper := golanghelpers.Go{Dir: p.Location}

	err = helper.BufGenerate(p.PluginLogger)
	if err != nil {
		return nil, fmt.Errorf("factory>create: go helper: cannot run buf generate: %v", err)
	}
	err = helper.ModTidy(p.PluginLogger)
	if err != nil {
		return nil, fmt.Errorf("factory>create: go helper: cannot run mod tidy: %v", err)
	}

	return &factoryv1.CreateResponse{
		Spec:      spec,
		Endpoints: endpoints,
	}, nil
}

func (p *Factory) Update(req *factoryv1.UpdateRequest) (*factoryv1.UpdateResponse, error) {
	defer p.PluginLogger.Catch()

	p.ServiceLogger.Info("Updating")

	err := templates.CopyAndApply(p.PluginLogger, templates.NewEmbeddedFileSystem(builder), shared.NewDir("templates/builder"),
		shared.NewDir(p.Local("builder")), nil)
	if err != nil {
		return nil, p.PluginLogger.Wrapf(err, "cannot copy and apply template")
	}

	helper := golanghelpers.Go{Dir: p.Location}
	err = helper.Update(p.PluginLogger)
	if err != nil {
		return nil, fmt.Errorf("factory>update: go helper: cannot run update: %v", err)
	}
	return &factoryv1.UpdateResponse{}, nil
}

func (p *Factory) Communicate(req *corev1.Engage) (*corev1.InformationRequest, error) {
	p.PluginLogger.DebugMe("factory communicate: %v", req)
	return p.Base.Communicate(req)
}

func (p *Service) InitEndpoints() ([]*corev1.Endpoint, error) {
	//p.GrpcEndpoint = &configurations.Endpoint{
	//	Name:        configurations.Grpc,
	//	Api:         &configurations.Api{Protocol: configurations.Grpc},
	//	Description: "Expose gRPC",
	//}
	//
	//p.PluginLogger.Debugf("initEndpoints: %v", p.Spec.CreateHttpEndpoint)
	//if p.Spec.CreateHttpEndpoint {
	//	p.RestEndpoint = &configurations.Endpoint{
	//		Name:        configurations.Http,
	//		Api:         &configurations.Api{Protocol: configurations.Http, Framework: configurations.RestFramework},
	//		Description: "Expose REST",
	//	}
	//}
	//
	//grpc, err := services.NewGrpcApi(p.GrpcEndpoint, p.Local("api.proto"))
	//if err != nil {
	//	return nil, shared.Wrapf(err, "cannot create grpc api")
	//}
	//
	//endpoints := []*corev1.Endpoint{grpc}
	//if p.RestEndpoint != nil {
	//	rest, err := services.NewOpenApi(p.RestEndpoint, p.Local("adapters/v1/swagger/api.swagger.json"))
	//	if err != nil {
	//		return nil, shared.Wrapf(err, "cannot create REST api")
	//	}
	//	endpoints = append(endpoints, rest)
	//}
	return nil, nil
}

//go:embed templates/factory
var factory embed.FS

//go:embed templates/builder
var builder embed.FS
