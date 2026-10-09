package kubernetes

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/j75689/anyship/internal/exectest"
)

func TestDoctor(t *testing.T) {
	f := &exectest.Fake{Answers: map[string]exectest.Answer{
		"kubectl version --client -o json": {Stdout: `{"clientVersion": {"gitVersion": "v1.33.9"}}`},
		"kubectl config current-context":   {Stdout: "orbstack\n"},
		"kubectl config get-contexts":      {Err: errors.New("exit status 1")},
		"docker buildx version":            {Stdout: "github.com/docker/buildx v0.33.0 abc\n"},
	}}
	c := New().Doctor(context.Background(), nil, f.Env(t.TempDir()))
	if len(c.Findings) != 0 || c.Tools[0].Version != "1.33.9" || c.Tools[0].Login != "orbstack" {
		t.Errorf("checkup = %+v", c)
	}
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}}, "targets": {"kubernetes": {"namespace": "apps", "context": "prod"}}}`)
	if c := New().Doctor(context.Background(), s, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"K8S_PREFLIGHT_CONTEXT"}) {
		t.Errorf("unknown context: %+v", c.Findings)
	}
	f.Answers["kubectl version --client -o json"] = exectest.Missing("kubectl")
	if c := New().Doctor(context.Background(), nil, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"K8S_PREFLIGHT_KUBECTL"}) {
		t.Errorf("no kubectl: %+v", c.Findings)
	}
}
