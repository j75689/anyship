package gcp

import (
	"context"
	"encoding/json"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

var _ adapter.Doctor = (*Adapter)(nil)

// Doctor checks gcloud, the account of the spec's configuration and docker
// buildx. The account is read from gcloud's config; no token is refreshed.
func (a *Adapter) Doctor(ctx context.Context, s *spec.Spec, env *adapter.Env) adapter.Checkup {
	var o struct {
		Configuration string `json:"configuration"`
	}
	if s != nil {
		_ = json.Unmarshal(s.Targets[Name], &o)
	}
	var opts adapter.ExecOptions
	if o.Configuration != "" {
		opts.Env = []string{"CLOUDSDK_ACTIVE_CONFIG_NAME=" + o.Configuration}
	}

	var c adapter.Checkup
	gcloud := adapter.Tool{Name: "gcloud", Need: "deploys to Cloud Run", Min: adapter.GCloudFloor.Min}
	out, _, err := adapter.Probe(ctx, env, opts, "gcloud", "--version")
	if err != nil {
		c.Findings = append(c.Findings, adapter.Missing(notInstalled, "gcloud", err))
	} else {
		gcloud.Found, gcloud.Version = true, gcloudVersionIn(out)
		c.Findings = append(c.Findings, adapter.GCloudFloor.Check(gcloud.Version, "GCP_PREFLIGHT_VERSION")...)
		gcloud.Login, _, _ = adapter.Probe(ctx, env, opts, "gcloud", "config", "get-value", "account")
		if gcloud.Login == "" {
			c.Findings = append(c.Findings, adapter.Finding{Level: adapter.Error, Code: "GCP_PREFLIGHT_AUTH", Message: "gcloud has no account set.", Hint: loginHint(o.Configuration)})
		}
	}
	buildx, findings := adapter.Buildx(ctx, env, adapter.BuildsFromSource(s), "GCP")
	c.Tools = append(c.Tools, gcloud, buildx)
	c.Findings = append(c.Findings, findings...)
	return c
}
