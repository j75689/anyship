package cloudflare

import (
	"context"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

var _ adapter.Doctor = (*Adapter)(nil)

// Doctor checks Node.js, wrangler and wrangler's login, with the checks
// apply's preflight runs. Unlike a deploy it never downloads wrangler: one
// npx can't find without downloading is reported, and its login isn't
// checked.
func (a *Adapter) Doctor(ctx context.Context, _ *spec.Spec, env *adapter.Env) adapter.Checkup {
	probe := npxProbe(ctx, env)
	t, findings := checkTools(probe)
	c := adapter.Checkup{Findings: findings, Tools: []adapter.Tool{
		{Name: "node", Need: "runs wrangler (through npx)", Found: t.nodeRuns, Version: t.node},
		{Name: "wrangler", Need: "deploys the Worker", Min: adapter.WranglerFloor.Min, Found: t.wranglerRuns, Version: t.wrangler},
	}}
	if !t.wranglerRuns {
		return c
	}
	wrangler := &c.Tools[1]
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
