package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

var _ adapter.Doctor = (*Adapter)(nil)

// Doctor checks kubectl, the context the spec deploys to and docker buildx.
// It reads the kubeconfig only; the cluster isn't contacted.
func (a *Adapter) Doctor(ctx context.Context, s *spec.Spec, env *adapter.Env) adapter.Checkup {
	var o struct {
		Context string `json:"context"`
	}
	if s != nil {
		_ = json.Unmarshal(s.Targets[Name], &o)
	}

	var c adapter.Checkup
	kubectl := adapter.Tool{Name: "kubectl", Need: "deploys to the cluster", Min: adapter.KubectlFloor.Min}
	if out, _, err := adapter.Probe(ctx, env, adapter.ExecOptions{}, "kubectl", "version", "--client", "-o", "json"); err != nil {
		c.Findings = append(c.Findings, adapter.Missing(notInstalled, "kubectl", err))
	} else {
		var v struct {
			ClientVersion struct {
				GitVersion string `json:"gitVersion"`
			} `json:"clientVersion"`
		}
		_ = json.Unmarshal([]byte(out), &v)
		kubectl.Found, kubectl.Version = true, adapter.VersionIn(v.ClientVersion.GitVersion)
		c.Findings = append(c.Findings, adapter.KubectlFloor.Check(kubectl.Version, "K8S_PREFLIGHT_VERSION")...)
		if o.Context == "" {
			kubectl.Login, _, _ = adapter.Probe(ctx, env, adapter.ExecOptions{}, "kubectl", "config", "current-context")
			if kubectl.Login == "" {
				c.Findings = append(c.Findings, adapter.Finding{Level: adapter.Error, Code: "K8S_PREFLIGHT_CONTEXT", Message: "The kubeconfig has no current context.",
					Hint: "Set spec.targets.kubernetes.context to one of `kubectl config get-contexts`, or `kubectl config use-context <name>`."})
			}
		} else if _, _, err := adapter.Probe(ctx, env, adapter.ExecOptions{}, "kubectl", "config", "get-contexts", o.Context, "-o", "name"); err != nil {
			c.Findings = append(c.Findings, adapter.Finding{Level: adapter.Error, Code: "K8S_PREFLIGHT_CONTEXT", Message: fmt.Sprintf("Context %s isn't in the kubeconfig.", o.Context),
				Hint: "Set spec.targets.kubernetes.context to one of `kubectl config get-contexts`, or leave it out to use the current one."})
		} else {
			kubectl.Login = o.Context
		}
	}
	buildx, findings := adapter.Buildx(ctx, env, adapter.BuildsFromSource(s), "K8S")
	c.Tools = append(c.Tools, kubectl, buildx)
	c.Findings = append(c.Findings, findings...)
	return c
}
