package cloudflare

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/internal/exectest"
)

func TestDoctor(t *testing.T) {
	f := &exectest.Fake{Answers: map[string]exectest.Answer{
		"node --version":                          {Stdout: "v22.1.0\n"},
		"npx --no-install wrangler --version":     {Stdout: "4.149.0\n"},
		"npx --no-install wrangler whoami --json": {Stdout: `{"loggedIn": true, "email": "dev@example.com"}`},
	}}
	c := New().Doctor(context.Background(), nil, f.Env(t.TempDir()))
	if len(c.Findings) != 0 || c.Tools[0].Version != "22.1.0" || c.Tools[1].Version != "4.149.0" || c.Tools[1].Login != "dev@example.com" {
		t.Errorf("checkup = %+v", c)
	}
	for _, call := range f.Calls {
		if slices.Contains([]string{"npx wrangler", "npx --yes"}, call.Line[:min(len(call.Line), 12)]) {
			t.Errorf("doctor may download wrangler: %s", call.Line)
		}
	}

	f.Answers["npx --no-install wrangler whoami --json"] = exectest.Answer{Stdout: `{"loggedIn": false}`, Err: errors.New("exit status 1")}
	if c := New().Doctor(context.Background(), nil, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"CF_PREFLIGHT_AUTH"}) {
		t.Errorf("logged out: %+v", c.Findings)
	}
	// Not in the project or npx's cache: a deploy downloads it, so that is no error.
	f.Answers["npx --no-install wrangler --version"] = exectest.Answer{Err: errors.New("exit status 1")}
	c = New().Doctor(context.Background(), nil, f.Env(t.TempDir()))
	if adapter.HasErrors(c.Findings) || !slices.Equal(exectest.Codes(c.Findings), []string{"CF_PREFLIGHT_WRANGLER"}) || c.Tools[1].Found {
		t.Errorf("wrangler not downloaded: %+v", c)
	}
	f.Answers["node --version"] = exectest.Missing("node")
	if c := New().Doctor(context.Background(), nil, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"CF_PREFLIGHT_NODE"}) || len(c.Tools) != 2 {
		t.Errorf("no node: %+v", c)
	}
}
