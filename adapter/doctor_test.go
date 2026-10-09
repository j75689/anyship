package adapter_test

import (
	"context"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/internal/exectest"
	"github.com/j75689/anyship/internal/spectest"
	"github.com/j75689/anyship/spec"
)

func TestBuildx(t *testing.T) {
	builds, err := spectest.Parse(`{"name": "a", "services": {"web": {"kind": "server", "start": "node a.js"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	pulls, err := spectest.Parse(`{"name": "a", "services": {"web": {"kind": "server", "image": "nginx"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	ok := &exectest.Fake{Answers: map[string]exectest.Answer{"docker buildx version": {Stdout: "github.com/docker/buildx v0.33.0 f7897eb\n"}}}
	if tool, findings := adapter.Buildx(context.Background(), ok.Env(""), builds, "X_DOCKER"); !tool.Found || tool.Version != "0.33.0" || len(findings) != 0 {
		t.Errorf("found: %+v %+v", tool, findings)
	}
	missing := &exectest.Fake{Answers: map[string]exectest.Answer{"docker": exectest.Missing("docker")}}
	for name, tc := range map[string]struct {
		s    *spec.Spec
		want adapter.Level
	}{"builds": {builds, adapter.Error}, "pulls": {pulls, adapter.Warning}, "no spec": {nil, adapter.Warning}} {
		tool, findings := adapter.Buildx(context.Background(), missing.Env(""), tc.s, "X_DOCKER")
		if tool.Found || len(findings) != 1 || findings[0].Level != tc.want || findings[0].Code != "X_DOCKER" {
			t.Errorf("%s: %+v %+v", name, tool, findings)
		}
	}
}

func TestMissingAndVersionIn(t *testing.T) {
	missing := adapter.Finding{Code: "X", Message: "x isn't installed."}
	if got := adapter.Missing(missing, "x", exectest.Missing("x").Err); got.Message != missing.Message {
		t.Errorf("not installed: %q", got.Message)
	}
	if got := adapter.Missing(missing, "x", context.Canceled); got.Message != "x is installed but doesn't run: context canceled." {
		t.Errorf("broken: %q", got.Message)
	}
	for in, want := range map[string]string{"Google Cloud SDK 587.0.0": "587.0.0", "v1.33.9": "1.33.9", "aws-cli/2.27.0 Python/3.13.1": "2.27.0", "none": ""} {
		if got := adapter.VersionIn(in); got != want {
			t.Errorf("VersionIn(%q) = %q, want %q", in, got, want)
		}
	}
}
