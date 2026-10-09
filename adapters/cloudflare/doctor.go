package cloudflare

import (
	"context"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

var _ adapter.Doctor = (*Adapter)(nil)

// Doctor checks Node.js, wrangler and wrangler's login. Unlike a deploy it
// never downloads wrangler: a wrangler npx can't find without downloading
// is reported, and its login isn't checked.
func (a *Adapter) Doctor(ctx context.Context, _ *spec.Spec, env *adapter.Env) (c adapter.Checkup) {
	probe := func(name string, args ...string) (string, error) {
		out, _, err := adapter.Probe(ctx, env, adapter.ExecOptions{}, name, args...)
		return out, err
	}
	node := adapter.Tool{Name: "node", Need: "runs wrangler (through npx)"}
	wrangler := adapter.Tool{Name: "wrangler", Need: "deploys the Worker"}
	defer func() { c.Tools = []adapter.Tool{node, wrangler} }()

	out, err := probe("node", "--version")
	if err != nil {
		c.Findings = append(c.Findings, adapter.Missing(adapter.Finding{Level: adapter.Error, Code: "CF_PREFLIGHT_NODE",
			Message: "Node.js isn't installed on this machine.", Hint: installNode}, "node", err))
		return c
	}
	node.Found, node.Version = true, adapter.VersionIn(out)

	out, err = probe("npx", "--no-install", "wrangler", "--version")
	if adapter.NotInstalled(err) {
		c.Findings = append(c.Findings, adapter.Finding{Level: adapter.Error, Code: "CF_PREFLIGHT_NODE", Message: "npx isn't installed on this machine.", Hint: installNode})
		return c
	}
	if err != nil {
		c.Findings = append(c.Findings, adapter.Finding{Level: adapter.Info, Code: "CF_PREFLIGHT_WRANGLER",
			Message: "wrangler isn't installed in this project or npx's cache; npx downloads it on the first deploy, and its login is checked then.",
			Hint:    "Add it to the project with `npm install --save-dev wrangler` to pin its version."})
		return c
	}
	wrangler.Found, wrangler.Version = true, adapter.VersionIn(out)
	ok, email := whoami(probe, "npx", "--no-install", "wrangler")
	switch {
	case !ok:
		c.Findings = append(c.Findings, notLoggedIn)
	case email != "":
		wrangler.Login = email
	default:
		wrangler.Login = "logged in"
	}
	return c
}
