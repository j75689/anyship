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

// preflight checks that Node.js and wrangler run on this machine and that
// wrangler is logged in, before anything is built or deployed.
func preflight(ctx context.Context, env *adapter.Env) []adapter.Finding {
	var findings []adapter.Finding
	add := func(level adapter.Level, code, message, hint string) {
		findings = append(findings, adapter.Finding{Level: level, Code: code, Message: message, Hint: hint})
	}
	// An empty stdin keeps npx from asking whether to download wrangler; it
	// downloads it, as the deploy itself would.
	probe := func(name string, args ...string) (string, error) {
		var out, errOut bytes.Buffer
		err := env.Exec(ctx, adapter.ExecOptions{Dir: env.Dir, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut}, name, args...)
		if err != nil && !adapter.NotInstalled(err) {
			if lines := strings.Split(strings.TrimSpace(errOut.String()), "\n"); lines[len(lines)-1] != "" {
				err = fmt.Errorf("%w: %s", err, strings.TrimSpace(lines[len(lines)-1]))
			}
		}
		return strings.TrimSpace(out.String()), err
	}

	const installNode = "Install Node.js 18 or newer (https://nodejs.org/), which brings npx; wrangler runs on it."
	if _, err := probe("node", "--version"); adapter.NotInstalled(err) {
		add(adapter.Error, "CF_PREFLIGHT_NODE", "Node.js isn't installed on this machine.", installNode)
		return findings
	} else if err != nil {
		add(adapter.Error, "CF_PREFLIGHT_NODE", fmt.Sprintf("Node.js doesn't run on this machine: %v.", err), installNode)
		return findings
	}
	out, err := probe("npx", "wrangler", "--version")
	switch {
	case adapter.NotInstalled(err):
		add(adapter.Error, "CF_PREFLIGHT_NODE", "npx isn't installed on this machine.", installNode)
		return findings
	case err != nil:
		add(adapter.Error, "CF_PREFLIGHT_WRANGLER", fmt.Sprintf("wrangler doesn't run through npx: %v.", err),
			"Add it to the project with `npm install --save-dev wrangler`, or check that npm can reach its registry.")
		return findings
	}
	version := versionPattern.FindString(out)

	if !loggedIn(probe) {
		add(adapter.Error, "CF_PREFLIGHT_AUTH", "wrangler isn't logged in to Cloudflare.",
			"Run `npx wrangler login`, or set CLOUDFLARE_API_TOKEN (and CLOUDFLARE_ACCOUNT_ID) for a CI deploy.")
		return findings
	}
	add(adapter.Info, "CF_PREFLIGHT_OK", fmt.Sprintf("wrangler %s runs and is logged in.", version), "")
	return findings
}

// loggedIn asks `wrangler whoami`. Its plain form exits 0 even when logged
// out, so the JSON form (wrangler 4) is read first, and the text of the
// plain form is the fallback for wranglers that don't have --json.
func loggedIn(probe func(string, ...string) (string, error)) bool {
	out, _ := probe("npx", "wrangler", "whoami", "--json")
	var who struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if json.Unmarshal([]byte(out), &who) == nil && who.LoggedIn != nil {
		return *who.LoggedIn
	}
	out, err := probe("npx", "wrangler", "whoami")
	return err == nil && !strings.Contains(out, "not authenticated")
}
