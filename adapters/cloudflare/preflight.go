package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/j75689/anyship/adapter"
)

const installNode = "Install Node.js 18 or newer (https://nodejs.org/), which brings npx; wrangler runs on it."

var notLoggedIn = adapter.Finding{Level: adapter.Error, Code: "CF_PREFLIGHT_AUTH", Message: "wrangler isn't logged in to Cloudflare.",
	Hint: "Run `npx wrangler login`, or set CLOUDFLARE_API_TOKEN (and CLOUDFLARE_ACCOUNT_ID) for a CI deploy."}

// tools is what the cloudflare target drives on this machine, as checkTools
// found it: whether Node.js and wrangler run, and their versions.
type tools struct {
	nodeRuns, wranglerRuns bool
	node, wrangler         string
}

// probeFunc runs a command and returns its trimmed stdout.
type probeFunc func(name string, args ...string) (string, error)

// npxProbe returns a probe that runs a command through env with an empty
// stdin, so nothing waits on the terminal, and folds npx's error message
// into the error of a command that failed.
func npxProbe(ctx context.Context, env *adapter.Env) probeFunc {
	return func(name string, args ...string) (string, error) {
		out, errOut, err := adapter.Probe(ctx, env, adapter.ExecOptions{}, name, args...)
		if err != nil && !adapter.NotInstalled(err) {
			if msg := npxError(errOut); msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
		}
		return out, err
	}
}

// checkTools checks that Node.js, npx and wrangler run on this machine, and
// wrangler's version against its floor. apply's preflight and doctor share
// it, so both read a failure the same way. It never downloads wrangler
// (`npx --no-install`, which still asks the registry which version it would
// install, so it needs the network): a wrangler that isn't in the project or
// npx's cache yet is an Info finding, and the deploy's own npx downloads it,
// asking first. The checks that need wrangler are the caller's, once
// wranglerRuns.
func checkTools(probe probeFunc) (tools, []adapter.Finding) {
	var t tools
	finding := func(level adapter.Level, code, message, hint string) []adapter.Finding {
		return []adapter.Finding{{Level: level, Code: code, Message: message, Hint: hint}}
	}

	out, err := probe("node", "--version")
	switch {
	case adapter.NotInstalled(err):
		return t, finding(adapter.Error, "CF_PREFLIGHT_NODE", "Node.js isn't installed on this machine.", installNode)
	case err != nil:
		return t, finding(adapter.Error, "CF_PREFLIGHT_NODE", fmt.Sprintf("Node.js doesn't run on this machine: %v.", err), installNode)
	}
	t.nodeRuns, t.node = true, adapter.VersionIn(out)

	out, err = probe("npx", "--no-install", "wrangler", "--version")
	switch {
	case adapter.NotInstalled(err):
		return t, finding(adapter.Error, "CF_PREFLIGHT_NODE", "npx isn't installed on this machine.", installNode)
	case err != nil && strings.Contains(err.Error(), "missing packages"):
		return t, finding(adapter.Info, "CF_PREFLIGHT_WRANGLER", "wrangler isn't in this project or npx's cache yet, so its version and login weren't checked; npx downloads it for the deploy.",
			"Add it to the project with `npm install --save-dev wrangler` to pin its version.")
	case err != nil:
		return t, finding(adapter.Error, "CF_PREFLIGHT_WRANGLER", fmt.Sprintf("wrangler doesn't run through npx: %v.", err),
			"Reinstall it with `npm install --save-dev wrangler`.")
	}
	t.wranglerRuns, t.wrangler = true, adapter.VersionIn(out)
	return t, adapter.WranglerFloor.Check(t.wrangler, "CF_PREFLIGHT_VERSION")
}

// preflight checks that Node.js and wrangler run on this machine and that
// wrangler is logged in, before anything is built or deployed. It changes
// nothing a dry run didn't already need: checkTools never downloads
// wrangler, and `wrangler deploy --dry-run` needs no login, so with dryRun a
// missing login is a warning about the real deploy instead of an error.
func preflight(ctx context.Context, env *adapter.Env, dryRun bool) []adapter.Finding {
	probe := npxProbe(ctx, env)
	t, findings := checkTools(probe)
	if !t.wranglerRuns {
		return findings
	}
	if ok, _ := whoami(probe, "npx", "--no-install", "wrangler"); !ok {
		f := notLoggedIn
		if dryRun {
			f.Level, f.Message = adapter.Warning, "wrangler isn't logged in to Cloudflare; a dry run doesn't need it, a deploy does."
		}
		return append(findings, f)
	}
	return append(findings, adapter.Finding{Level: adapter.Info, Code: "CF_PREFLIGHT_OK", Message: fmt.Sprintf("wrangler %s runs and is logged in.", t.wrangler)})
}

// errorLine is how Node.js names an uncaught error, before its stack:
// `Error: …`, `TypeError: …`, `Error [ERR_MODULE_NOT_FOUND]: …`.
var errorLine = regexp.MustCompile(`^\w*Error\b`)

// npxError picks npm's or the command's message out of npx's error output:
// the first npm error line (`npm error` since npm 10, `npm ERR!` before),
// else the line that names the error (the stack follows it, so the last
// line would be a frame), else the last line.
func npxError(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"npm error ", "npm ERR! "} {
			if msg, ok := strings.CutPrefix(line, prefix); ok {
				return msg
			}
		}
	}
	for _, line := range lines {
		if line = strings.TrimSpace(line); errorLine.MatchString(line) {
			return line
		}
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// whoami asks wrangler, run as npx and args, who is logged in. Plain
// `wrangler whoami` exits 0 even when logged out, so the JSON form
// (wrangler 4) is read first, and the text of the plain form is the
// fallback for wranglers that don't have --json; that one gives no email.
func whoami(probe probeFunc, npx string, args ...string) (loggedIn bool, email string) {
	out, _ := probe(npx, append(args, "whoami", "--json")...)
	var who struct {
		LoggedIn *bool  `json:"loggedIn"`
		Email    string `json:"email"`
	}
	if json.Unmarshal([]byte(out), &who) == nil && who.LoggedIn != nil {
		return *who.LoggedIn, who.Email
	}
	out, err := probe(npx, append(args, "whoami")...)
	return err == nil && !strings.Contains(out, "not authenticated"), ""
}
