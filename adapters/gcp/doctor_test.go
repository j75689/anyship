package gcp

import (
	"context"
	"slices"
	"testing"

	"github.com/j75689/anyship/internal/exectest"
)

func TestDoctor(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}},
		"targets": {"gcp": {"project": "my-project", "region": "us-central1", "configuration": "work"}}}`)
	f := &exectest.Fake{Answers: map[string]exectest.Answer{
		"gcloud --version":                {Stdout: "Google Cloud SDK 587.0.0\nbeta 2026.09.25\n"},
		"gcloud config get-value account": {Stdout: "dev@example.com\n"},
		"docker buildx version":           {Stdout: "github.com/docker/buildx v0.33.0 abc\n"},
	}}
	c := New().Doctor(context.Background(), s, f.Env(t.TempDir()))
	if len(c.Findings) != 0 || c.Tools[0].Version != "587.0.0" || c.Tools[0].Login != "dev@example.com" || !c.Tools[1].Found {
		t.Errorf("checkup = %+v", c)
	}
	for _, call := range f.Calls {
		if call.Line == "gcloud config get-value account" && !slices.Equal(call.Opts.Env, []string{"CLOUDSDK_ACTIVE_CONFIG_NAME=work"}) {
			t.Errorf("the spec's configuration wasn't used: %+v", call.Opts.Env)
		}
	}

	f.Answers["gcloud config get-value account"] = exectest.Answer{}
	if c := New().Doctor(context.Background(), s, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"GCP_PREFLIGHT_AUTH"}) {
		t.Errorf("no account: %+v", c.Findings)
	}
	f.Answers["gcloud --version"] = exectest.Missing("gcloud")
	if c := New().Doctor(context.Background(), nil, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"GCP_PREFLIGHT_GCLOUD"}) || c.Tools[0].Found {
		t.Errorf("no gcloud: %+v", c)
	}
}
