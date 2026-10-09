package adapter_test

import (
	"context"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/internal/exectest"
	"github.com/j75689/anyship/internal/spectest"
)

func TestBuildx(t *testing.T) {
	ok := &exectest.Fake{Answers: map[string]exectest.Answer{"docker buildx version": {Stdout: "github.com/docker/buildx v0.33.0 f7897eb\n"}}}
	if tool, findings := adapter.Buildx(context.Background(), ok.Env(""), true, "X"); !tool.Found || tool.Version != "0.33.0" || tool.Min == "" || len(findings) != 0 {
		t.Errorf("found: %+v %+v", tool, findings)
	}
	old := &exectest.Fake{Answers: map[string]exectest.Answer{"docker buildx version": {Stdout: "github.com/docker/buildx v0.5.1 11057da\n"}}}
	if _, findings := adapter.Buildx(context.Background(), old.Env(""), true, "X"); len(findings) != 1 || findings[0].Code != "X_PREFLIGHT_VERSION" ||
		findings[0].Level != adapter.Warning || findings[0].Message != "anyship needs docker buildx 0.6.0 or newer for `--metadata-file`; this machine has 0.5.1." {
		t.Errorf("old: %+v", findings)
	}
	missing := &exectest.Fake{Answers: map[string]exectest.Answer{"docker buildx version": exectest.Missing("docker")}}
	for needed, want := range map[bool]adapter.Level{true: adapter.Error, false: adapter.Warning} {
		tool, findings := adapter.Buildx(context.Background(), missing.Env(""), needed, "X")
		if tool.Found || len(findings) != 1 || findings[0].Level != want || findings[0].Code != "X_PREFLIGHT_DOCKER" {
			t.Errorf("needed %v: %+v %+v", needed, tool, findings)
		}
	}
}

func TestBuildsFromSource(t *testing.T) {
	for src, want := range map[string]bool{
		`{"name": "a", "services": {"web": {"kind": "server", "start": "node a.js"}}}`: true,
		`{"name": "a", "services": {"web": {"kind": "server", "image": "nginx"}}}`:     false,
	} {
		s, err := spectest.Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		if got := adapter.BuildsFromSource(s); got != want {
			t.Errorf("%s: %v", src, got)
		}
	}
	if adapter.BuildsFromSource(nil) {
		t.Error("no spec builds")
	}
}

func TestMissingAndVersionIn(t *testing.T) {
	missing := adapter.Finding{Code: "X", Message: "x isn't installed."}
	if got := adapter.Missing(missing, "x", exectest.Missing("x").Err); got.Message != missing.Message {
		t.Errorf("not installed: %q", got.Message)
	}
	if got := adapter.Missing(missing, "x", context.Canceled); got.Message != "x is installed but doesn't run: context canceled." || got.Hint != "Run `x` by hand to see why, or reinstall it." {
		t.Errorf("broken: %+v", got)
	}
	for in, want := range map[string]string{"Google Cloud SDK 587.0.0": "587.0.0", "v1.33.9": "1.33.9", "aws-cli/2.27.0 Python/3.13.1": "2.27.0", "none": ""} {
		if got := adapter.VersionIn(in); got != want {
			t.Errorf("VersionIn(%q) = %q, want %q", in, got, want)
		}
	}
}
