package cloudflare

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
		"node --version":                          {Stdout: "v22.1.0\n"},
		"npx --no-install wrangler --version":     {Stdout: " ⛅️ wrangler 4.149.0\n"},
		"npx --no-install wrangler whoami --json": {Stdout: `{"loggedIn": true, "email": "dev@example.com"}`},
	}}
	c := New().Doctor(context.Background(), nil, f.Env(t.TempDir()))
	if len(c.Findings) != 0 || c.Dependencies[0].Version != "22.1.0" || c.Dependencies[1].Version != "4.149.0" || c.Dependencies[1].Login != "dev@example.com" {
		t.Errorf("checkup = %+v", c)
	}
	for _, call := range f.Calls {
		if slices.Contains([]string{"npx wrangler", "npx --yes"}, call.Line[:min(len(call.Line), 12)]) {
			t.Errorf("doctor may download wrangler: %s", call.Line)
		}
		if call.Opts.Stdin == nil {
			t.Errorf("%s may wait on the terminal", call.Line)
		}
	}

	// An old wrangler is a warning; its login is still read.
	f.Answers["npx --no-install wrangler --version"] = exectest.Answer{Stdout: "3.80.0\n"}
	if c := New().Doctor(context.Background(), nil, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"CF_PREFLIGHT_VERSION"}) ||
		c.Findings[0].Level != adapter.Warning || c.Dependencies[1].Min != "3.91.0" || c.Dependencies[1].Login != "dev@example.com" {
		t.Errorf("old wrangler: %+v", c)
	}
	// A wrangler 3 has no `whoami --json`; the plain form's text is read.
	f.Answers["npx --no-install wrangler whoami --json"] = exectest.Answer{Err: errors.New("exit status 1")}
	f.Answers["npx --no-install wrangler whoami"] = exectest.Answer{Stdout: "Getting User settings...\n👋 You are logged in with an OAuth Token.\n"}
	if c := New().Doctor(context.Background(), nil, f.Env(t.TempDir())); adapter.HasErrors(c.Findings) || c.Dependencies[1].Login != "logged in" {
		t.Errorf("wrangler 3 logged in: %+v", c)
	}
	f.Answers["npx --no-install wrangler whoami"] = exectest.Answer{Stdout: "You are not authenticated. Please run `wrangler login`.\n"}
	if c := New().Doctor(context.Background(), nil, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"CF_PREFLIGHT_VERSION", "CF_PREFLIGHT_AUTH"}) || c.Dependencies[1].Login != "" {
		t.Errorf("wrangler 3 logged out: %+v", c)
	}

	f.Answers["npx --no-install wrangler --version"] = exectest.Answer{Stdout: "4.149.0\n"}
	f.Answers["npx --no-install wrangler whoami --json"] = exectest.Answer{Stdout: `{"loggedIn": false}`, Err: errors.New("exit status 1")}
	if c := New().Doctor(context.Background(), nil, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"CF_PREFLIGHT_AUTH"}) {
		t.Errorf("logged out: %+v", c.Findings)
	}

	// Not in the project or npx's cache: a deploy downloads it, so that is no error...
	f.Answers["npx --no-install wrangler --version"] = exectest.Answer{Err: errors.New("exit status 1"),
		Stderr: "npm error npx canceled due to missing packages and no YES option: [\"wrangler@4.149.0\"]\nnpm error A complete log of this run can be found in: /x/_logs/1-debug-0.log\n"}
	c = New().Doctor(context.Background(), nil, f.Env(t.TempDir()))
	if adapter.HasErrors(c.Findings) || !slices.Equal(exectest.Codes(c.Findings), []string{"CF_PREFLIGHT_WRANGLER"}) || !c.Dependencies[0].Found || c.Dependencies[1].Found {
		t.Errorf("wrangler not downloaded: %+v", c)
	}
	// ...but a wrangler that is installed and fails is, as it is for apply.
	f.Answers["npx --no-install wrangler --version"] = exectest.Answer{Err: errors.New("exit status 1"), Stderr: "Error: Cannot find module 'undici'\n"}
	c = New().Doctor(context.Background(), nil, f.Env(t.TempDir()))
	if !slices.Equal(exectest.Codes(c.Findings), []string{"CF_PREFLIGHT_WRANGLER"}) || c.Findings[0].Level != adapter.Error ||
		!strings.Contains(c.Findings[0].Message, "Cannot find module 'undici'") || c.Dependencies[1].Found {
		t.Errorf("broken wrangler: %+v", c)
	}

	f.Answers["npx --no-install wrangler --version"] = exectest.Missing("npx")
	if c := New().Doctor(context.Background(), nil, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"CF_PREFLIGHT_NODE"}) ||
		c.Findings[0].Message != "npx isn't installed on this machine." || !c.Dependencies[0].Found || c.Dependencies[1].Found {
		t.Errorf("no npx: %+v", c)
	}
	f.Answers["node --version"] = exectest.Missing("node")
	if c := New().Doctor(context.Background(), nil, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"CF_PREFLIGHT_NODE"}) || len(c.Dependencies) != 2 || c.Dependencies[0].Found {
		t.Errorf("no node: %+v", c)
	}
}
