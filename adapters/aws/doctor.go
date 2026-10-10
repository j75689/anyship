package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

var _ adapter.Doctor = (*Adapter)(nil)

// Doctor checks the aws CLI, the credentials of the spec's profile and
// docker buildx. `aws configure list` finds the credentials without asking
// AWS, so an expired SSO session still passes here and fails preflight.
func (a *Adapter) Doctor(ctx context.Context, s *spec.Spec, env *adapter.Env) adapter.Checkup {
	var o struct {
		Profile string `json:"profile"`
	}
	if s != nil {
		_ = json.Unmarshal(s.Targets[Name], &o)
	}

	var c adapter.Checkup
	cli := adapter.Dependency{Name: "aws", Need: "deploys to ECS Express Mode", Min: adapter.AWSFloor.Min}
	out, errOut, err := adapter.Probe(ctx, env, adapter.ExecOptions{}, "aws", "--version")
	if err != nil {
		c.Findings = append(c.Findings, adapter.Missing(notInstalled, "aws", err))
	} else {
		cli.Found, cli.Version = true, cliVersionIn(out, errOut)
		c.Findings = append(c.Findings, adapter.AWSFloor.Check(cli.Version, "AWS_PREFLIGHT_VERSION")...)
		args := []string{"configure", "list"}
		if o.Profile != "" {
			args = append(args, "--profile", o.Profile)
		}
		out, errOut, err := adapter.Probe(ctx, env, adapter.ExecOptions{}, "aws", args...)
		kind, found := credentials(out)
		switch {
		case err != nil:
			c.Findings = append(c.Findings, adapter.Finding{Level: adapter.Error, Code: "AWS_PREFLIGHT_AUTH",
				Message: fmt.Sprintf("The aws CLI can't read its configuration: %s", lastLine(errOut, err)), Hint: loginHint})
		case !found:
			// The output has no access_key row anyship can read: say so rather
			// than pass a login that wasn't checked.
			check := "aws sts get-caller-identity"
			if o.Profile != "" {
				check += " --profile " + o.Profile
			}
			c.Findings = append(c.Findings, adapter.Finding{Level: adapter.Warning, Code: "AWS_PREFLIGHT_AUTH",
				Message: "anyship couldn't tell from `aws configure list` whether the aws CLI has credentials.", Hint: fmt.Sprintf("Check with `%s`, as apply's preflight does.", check)})
		case found && kind == "":
			c.Findings = append(c.Findings, adapter.Finding{Level: adapter.Error, Code: "AWS_PREFLIGHT_AUTH", Message: "The aws CLI has no credentials.", Hint: loginHint})
		case found:
			cli.Login = fmt.Sprintf("%s (%s)", cmpString(o.Profile, "default"), kind)
		}
	}
	buildx, findings := adapter.Buildx(ctx, env, adapter.BuildsFromSource(s), "AWS")
	c.Dependencies = append(c.Dependencies, cli, buildx)
	c.Findings = append(c.Findings, findings...)
	return c
}

// credentials reads where the access key comes from out of `aws configure
// list`: its type, such as env or sso, or "" when it isn't set. found is
// false when the output has no access_key row. Newer CLIs separate the
// columns with " : ", older ones with spaces only.
func credentials(out string) (kind string, found bool) {
	for _, line := range strings.Split(out, "\n") {
		var cols []string
		if strings.Contains(line, " : ") {
			for _, col := range strings.Split(line, " : ") {
				cols = append(cols, strings.Trim(col, " :"))
			}
		} else {
			cols = strings.Fields(line)
		}
		if len(cols) < 3 || cols[0] != "access_key" {
			continue
		}
		if cols[1] == "<not" || cols[1] == "<not set>" {
			return "", true
		}
		return cols[2], true
	}
	return "", false
}

func lastLine(text string, err error) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return last
	}
	return err.Error()
}

func cmpString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
