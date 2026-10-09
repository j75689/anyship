package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/j75689/anyship/adapter"
)

var versionPattern = regexp.MustCompile(`\d+\.\d+\.\d+`)

const installNode = "Install Node.js 18 or newer (https://nodejs.org/), which brings npx; wrangler runs on it."

var notLoggedIn = adapter.Finding{Level: adapter.Error, Code: "CF_PREFLIGHT_AUTH", Message: "wrangler isn't logged in to Cloudflare.",
	Hint: "Run `npx wrangler login`, or set CLOUDFLARE_API_TOKEN (and CLOUDFLARE_ACCOUNT_ID) for a CI deploy."}

// preflight checks that Node.js and wrangler run on this machine and that
// wrangler is logged in, before anything is built or deployed. It changes
// nothing a dry run didn't already need:
//   - it never downloads wrangler (npx --no-install). A wrangler that isn't
//     in the project or npx's cache yet is left to the deploy's own npx,
//     which asks before downloading it, and the checks that need it are
//     skipped;
//   - `wrangler deploy --dry-run` needs no login, so with dryRun a missing
//     login is a warning about the real deploy instead of an error.
func preflight(ctx context.Context, env *adapter.Env, dryRun bool) []adapter.Finding {
	var findings []adapter.Finding
	add := func(level adapter.Level, code, message, hint string) {
		findings = append(findings, adapter.Finding{Level: level, Code: code, Message: message, Hint: hint})
	}
	// An empty stdin keeps a probe from waiting on the terminal.
	probe := func(name string, args ...string) (string, error) {
		var out, errOut bytes.Buffer
		err := env.Exec(ctx, adapter.ExecOptions{Dir: env.Dir, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut}, name, args...)
		if err != nil && !adapter.NotInstalled(err) {
			if msg := npxError(errOut.String()); msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
		}
		return strings.TrimSpace(out.String()), err
	}

	if _, err := probe("node", "--version"); adapter.NotInstalled(err) {
		add(adapter.Error, "CF_PREFLIGHT_NODE", "Node.js isn't installed on this machine.", installNode)
		return findings
	} else if err != nil {
		add(adapter.Error, "CF_PREFLIGHT_NODE", fmt.Sprintf("Node.js doesn't run on this machine: %v.", err), installNode)
		return findings
	}
	out, err := probe("npx", "--no-install", "wrangler", "--version")
	switch {
	case adapter.NotInstalled(err):
		add(adapter.Error, "CF_PREFLIGHT_NODE", "npx isn't installed on this machine.", installNode)
		return findings
	case err != nil && strings.Contains(err.Error(), "missing packages"):
		add(adapter.Info, "CF_PREFLIGHT_WRANGLER", "wrangler isn't in this project or npx's cache yet, so its version and login weren't checked; npx downloads it for the deploy.",
			"Add it to the project with `npm install --save-dev wrangler` to pin its version.")
		return findings
	case err != nil:
		add(adapter.Error, "CF_PREFLIGHT_WRANGLER", fmt.Sprintf("wrangler doesn't run through npx: %v.", err),
			"Reinstall it with `npm install --save-dev wrangler`.")
		return findings
	}
	version := versionPattern.FindString(out)

	if ok, _ := whoami(probe, "npx", "--no-install", "wrangler"); !ok {
		f := notLoggedIn
		if dryRun {
			f.Level, f.Message = adapter.Warning, "wrangler isn't logged in to Cloudflare; a dry run doesn't need it, a deploy does."
		}
		return append(findings, f)
	}
	add(adapter.Info, "CF_PREFLIGHT_OK", fmt.Sprintf("wrangler %s runs and is logged in.", version), "")
	return findings
}

// npxError picks npm's or the command's message out of npx's error output:
// the first "npm error" line, else the last line.
func npxError(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for _, line := range lines {
		if msg, ok := strings.CutPrefix(strings.TrimSpace(line), "npm error "); ok {
			return msg
		}
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// whoami asks wrangler, run as npx and args, who is logged in. Plain
// `wrangler whoami` exits 0 even when logged out, so the JSON form
// (wrangler 4) is read first, and the text of the plain form is the
// fallback for wranglers that don't have --json; that one gives no email.
func whoami(probe func(string, ...string) (string, error), npx string, args ...string) (loggedIn bool, email string) {
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
