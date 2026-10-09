package vps

import (
	"context"
	"slices"
	"testing"

	"github.com/j75689/anyship/internal/exectest"
)

func TestDoctor(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}}, "targets": {"vps": {"host": "deploy@203.0.113.10"}}}`)
	f := &exectest.Fake{Answers: map[string]exectest.Answer{"ssh -V": {Stderr: "OpenSSH_10.3p1, LibreSSL 3.3.6\n"}}}
	c := New().Doctor(context.Background(), s, f.Env(t.TempDir()))
	if len(c.Findings) != 0 || c.Tools[0].Version != "10.3p1" || c.Tools[0].Login != "deploy@203.0.113.10" {
		t.Errorf("checkup = %+v", c)
	}
	f.Answers["ssh -V"] = exectest.Missing("ssh")
	if c := New().Doctor(context.Background(), s, f.Env(t.TempDir())); !slices.Equal(exectest.Codes(c.Findings), []string{"VPS_PREFLIGHT_SSH"}) || c.Tools[0].Login != "" {
		t.Errorf("no ssh: %+v", c)
	}
}
