package kubernetes

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/internal/exectest"
)

func TestDoctor(t *testing.T) {
	f := &exectest.Fake{Answers: map[string]exectest.Answer{
		"kubectl version --client -o json": {Stdout: `{"clientVersion": {"gitVersion": "v1.33.9"}}`},
		"kubectl config current-context":   {Stdout: "orbstack\n"},
		"kubectl config get-contexts":      {Err: errors.New("exit status 1")},
		"kubectl config get-contexts prod": {Stdout: "prod\n"},
		"docker buildx version":            {Stdout: "github.com/docker/buildx v0.33.0 abc\n"},
	}}
	c := New().Doctor(context.Background(), nil, f.Env(t.TempDir()))
	if len(c.Findings) != 0 || c.Dependencies[0].Version != "1.33.9" || c.Dependencies[0].Login != "orbstack" || c.Dependencies[0].Min != "1.26" {
		t.Errorf("checkup = %+v", c)
	}

	// The spec's context is looked up in the kubeconfig.
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}}, "targets": {"kubernetes": {"namespace": "apps", "context": "prod"}}}`)
	if c := New().Doctor(context.Background(), s, f.Env(t.TempDir())); len(c.Findings) != 0 || c.Dependencies[0].Login != "prod" {
		t.Errorf("spec's context: %+v", c)
	}
	s = parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}}, "targets": {"kubernetes": {"namespace": "apps", "context": "staging"}}}`)
	if c := New().Doctor(context.Background(), s, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"K8S_PREFLIGHT_CONTEXT"}) ||
		!strings.Contains(c.Findings[0].Message, "staging") || c.Dependencies[0].Login != "" {
		t.Errorf("unknown context: %+v", c)
	}
	// Without one in the spec, the kubeconfig must have a current context.
	f.Answers["kubectl config current-context"] = exectest.Answer{Stderr: "error: current-context is not set\n", Err: errors.New("exit status 1")}
	if c := New().Doctor(context.Background(), nil, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"K8S_PREFLIGHT_CONTEXT"}) ||
		c.Findings[0].Message != "The kubeconfig has no current context." {
		t.Errorf("no current context: %+v", c.Findings)
	}
	f.Answers["kubectl config current-context"] = exectest.Answer{Stdout: "orbstack\n"}

	// An old kubectl is a warning.
	f.Answers["kubectl version --client -o json"] = exectest.Answer{Stdout: `{"clientVersion": {"gitVersion": "v1.24.3"}}`}
	if c := New().Doctor(context.Background(), nil, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"K8S_PREFLIGHT_VERSION"}) ||
		c.Findings[0].Level != adapter.Warning || !strings.Contains(c.Findings[0].Message, "kubectl 1.26 or newer") || c.Dependencies[0].Version != "1.24.3" {
		t.Errorf("old kubectl: %+v", c)
	}

	f.Answers["kubectl version --client -o json"] = exectest.Missing("kubectl")
	if c := New().Doctor(context.Background(), nil, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"K8S_PREFLIGHT_KUBECTL"}) || c.Dependencies[0].Found {
		t.Errorf("no kubectl: %+v", c)
	}
}
