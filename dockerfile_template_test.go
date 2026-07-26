package main

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDockerfileTemplateUsesPinnedMinimalImages(t *testing.T) {
	t.Parallel()

	template, err := fs.ReadFile(builderFS, "templates/builder/Dockerfile.tmpl")
	if err != nil {
		t.Fatalf("read Dockerfile template: %v", err)
	}
	source := string(template)

	if !strings.Contains(source, "FROM golang:{{ .GoVersion }}-alpine3.23@sha256:") {
		t.Fatal("builder image must use the exact Go version and an immutable digest")
	}
	if !strings.Contains(source, "FROM alpine:{{ .AlpineVersion }}@sha256:") {
		t.Fatal("runtime image must use the exact Alpine version and an immutable digest")
	}

	runtimeStart := strings.Index(source, "# Final stage")
	if runtimeStart < 0 {
		t.Fatal("runtime stage marker is missing")
	}
	runtimeStage := source[runtimeStart:]
	if strings.Contains(runtimeStage, "ca-certificates git") {
		t.Fatal("runtime image must not include git")
	}
	if !strings.Contains(runtimeStage, "RUN apk add --no-cache ca-certificates") {
		t.Fatal("runtime image must retain the CA bundle")
	}
}
