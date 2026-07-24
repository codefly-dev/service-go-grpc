package main

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	golanghelpers "github.com/codefly-dev/core/runners/golang"
)

func TestDockerfileHonorsCGOSetting(t *testing.T) {
	tmpl, err := template.ParseFS(builderFS, "templates/builder/Dockerfile.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		withCGO       bool
		wantToolchain bool
		wantSetting   string
	}{
		{name: "disabled", wantSetting: "CGO_ENABLED=0"},
		{name: "enabled", withCGO: true, wantToolchain: true, wantSetting: "CGO_ENABLED=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rendered bytes.Buffer
			err := tmpl.Execute(&rendered, golanghelpers.DockerTemplating{
				GoVersion:     "1.26",
				AlpineVersion: "3.21",
				ModuleRoot:    "code",
				BuildTarget:   "./cmd/server",
				WithCGO:       tc.withCGO,
			})
			if err != nil {
				t.Fatal(err)
			}
			output := rendered.String()
			if got := strings.Contains(output, "build-base"); got != tc.wantToolchain {
				t.Fatalf("build toolchain present = %v, want %v\n%s", got, tc.wantToolchain, output)
			}
			if !strings.Contains(output, tc.wantSetting) {
				t.Fatalf("Dockerfile does not contain %q\n%s", tc.wantSetting, output)
			}
		})
	}
}
