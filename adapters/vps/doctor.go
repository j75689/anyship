package vps

import (
	"context"
	"encoding/json"
	"regexp"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

var _ adapter.Doctor = (*Adapter)(nil)

var opensshRe = regexp.MustCompile(`OpenSSH_([0-9][0-9A-Za-z.]*)`)

// Doctor checks the local ssh client. Docker runs on the server, which a
// doctor doesn't contact; apply's preflight checks it there.
func (a *Adapter) Doctor(ctx context.Context, s *spec.Spec, env *adapter.Env) adapter.Checkup {
	var o struct {
		Host string `json:"host"`
	}
	if s != nil {
		_ = json.Unmarshal(s.Targets[Name], &o)
	}
	ssh := adapter.Dependency{Name: "ssh", Need: "runs Docker Compose on the server", Login: o.Host}
	// ssh -V prints its version to stderr.
	_, errOut, err := adapter.Probe(ctx, env, adapter.ExecOptions{}, "ssh", "-V")
	if err != nil {
		ssh.Login = ""
		return adapter.Checkup{Dependencies: []adapter.Dependency{ssh}, Findings: []adapter.Finding{adapter.Missing(adapter.Finding{Level: adapter.Error, Code: "VPS_PREFLIGHT_SSH",
			Message: "ssh isn't installed on this machine.", Hint: "Install an OpenSSH client; on Windows, the OpenSSH Client optional feature."}, "ssh", err)}}
	}
	ssh.Found, ssh.Version = true, adapter.VersionIn(errOut)
	if m := opensshRe.FindStringSubmatch(errOut); m != nil {
		ssh.Version = m[1]
	}
	return adapter.Checkup{Dependencies: []adapter.Dependency{ssh}}
}
