package main

import (
	"fmt"

	"github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

// The template owns these requirements. Native services retain musl and apk;
// static services use the same runtime input only to assemble files copied into
// scratch. A stable role lets one type-wide selection cover both service shapes.
func buildImageInputs() []*builderv0.BuildImageInput {
	return []*builderv0.BuildImageInput{
		{Role: "frontend", Reference: "docker.io/docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32", Variant: "dockerfile-v1"},
		{Role: "builder", Reference: "docker.io/library/golang:" + GoVersion + "-alpine3.23@sha256:8a5046497cda39860b832ffdbe8dd66018a0f05bb01352f533a36e7e2ced66a0", Variant: "go" + GoVersion + "-alpine3.23"},
		{Role: "runtime", Reference: "docker.io/library/alpine:" + AlpineVersion + "@" + AlpineDigest, Variant: "alpine3.23-musl"},
	}
}

// Image serves the same defaults to standalone template tests and emits only
// resolved selections in production. No cell registry is hardcoded here.
func (d dockerTemplating) Image(role string) (string, error) {
	inputs := d.ImageBindings
	if inputs == nil {
		inputs = buildImageInputs()
	}
	for _, input := range inputs {
		if input.Role == role {
			return input.Reference, nil
		}
	}
	return "", fmt.Errorf("unknown image role %q", role)
}

func resolveTemplateImages(d *dockerTemplating, bindings []*builderv0.BuildImageInput) error {
	inputs, err := services.ResolveBuildImages(buildImageInputs(), bindings)
	if err != nil {
		return err
	}
	d.ImageBindings = inputs
	return nil
}
