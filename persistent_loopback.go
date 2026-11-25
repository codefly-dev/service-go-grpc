package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"

	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	"github.com/codefly-dev/core/agents/services"
	builderv1 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v1"
	"github.com/codefly-dev/core/shared"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

// PersistentLoopbackSpec declares an exec-accessible workload with a single
// writer and durable storage. The image owns its listeners and readiness command.
type PersistentLoopbackSpec struct {
	Image        string   `yaml:"image"`
	UID          int64    `yaml:"uid"`
	GID          int64    `yaml:"gid"`
	MountPath    string   `yaml:"mount-path"`
	Storage      string   `yaml:"storage"`
	StorageClass string   `yaml:"storage-class,omitempty"`
	Readiness    []string `yaml:"readiness"`
	Liveness     []string `yaml:"liveness"`
}

func (s *PersistentLoopbackSpec) Validate() error {
	if s == nil {
		return nil
	}
	if s.Image == "scratch" {
		return fmt.Errorf("persistent-loopback requires a packaged application image")
	}
	if err := services.ValidateBuildImageInput(s.imageInput()); err != nil {
		return fmt.Errorf("persistent-loopback: %w", err)
	}
	if s.UID <= 0 || s.GID <= 0 || s.UID > 2147483647 || s.GID > 2147483647 {
		return fmt.Errorf("persistent-loopback requires positive uid and gid within the Linux ID range")
	}
	if !path.IsAbs(s.MountPath) || path.Clean(s.MountPath) != s.MountPath || s.MountPath == "/" ||
		s.MountPath == "/tmp" || strings.ContainsRune(s.MountPath, 0) {
		return fmt.Errorf("persistent-loopback requires a clean absolute mount-path distinct from / and /tmp")
	}
	quantity, err := resource.ParseQuantity(s.Storage)
	if err != nil || quantity.Sign() <= 0 {
		return fmt.Errorf("persistent-loopback requires a positive Kubernetes storage quantity")
	}
	if s.StorageClass != "" && len(validation.IsDNS1123Subdomain(s.StorageClass)) != 0 {
		return fmt.Errorf("persistent-loopback storage-class must be a Kubernetes DNS subdomain")
	}
	for _, probe := range []struct {
		name    string
		command []string
	}{{"readiness", s.Readiness}, {"liveness", s.Liveness}} {
		if len(probe.command) == 0 || strings.TrimSpace(probe.command[0]) == "" {
			return fmt.Errorf("persistent-loopback requires a %s command", probe.name)
		}
		for _, arg := range probe.command {
			if strings.ContainsRune(arg, 0) {
				return fmt.Errorf("persistent-loopback %s command contains a NUL", probe.name)
			}
		}
	}
	return nil
}

func (s *PersistentLoopbackSpec) imageInput() *builderv1.BuildImageInput {
	return &builderv1.BuildImageInput{Role: "application", Reference: s.Image, Variant: "persistent-loopback-v1"}
}

//go:embed packaged/templates/builder
var packagedFS embed.FS

func (s *Builder) buildPackagedImage(ctx context.Context, req *builderv1.BuildRequest, settings *PersistentLoopbackSpec) (*builderv1.BuildResponse, error) {
	builder := s.Base.Builder
	if err := settings.Validate(); err != nil {
		return builder.BuildError(err)
	}
	inputs, err := services.ResolveBuildImages([]*builderv1.BuildImageInput{settings.imageInput()}, req.GetBuildContext().GetDockerBuildContext().GetImageBindings())
	if err != nil {
		return builder.BuildError(err)
	}
	request, err := builder.DockerBuildRequest(ctx, req)
	if err != nil {
		return builder.BuildError(err)
	}
	image := builder.DockerImage(request)
	if !dockerhelpers.IsValidDockerImageName(image.Name) {
		return builder.BuildError(fmt.Errorf("invalid docker image name: %s", image.Name))
	}
	templates, err := fs.Sub(packagedFS, "packaged")
	if err != nil {
		return builder.BuildError(err)
	}
	output := req.GetOutputDirectory()
	emitted, err := services.PrepareRecipeDestination(templates, output)
	if err != nil {
		return builder.BuildError(err)
	}
	if err := builder.Templates(ctx, inputs[0], services.WithBuilder(templates).WithDestination("%s", output).WithOverride(shared.OverrideAll())); err != nil {
		return builder.BuildError(err)
	}
	return emitBuildPlan(builder, output, image, emitted)
}
