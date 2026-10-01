package vps

import (
	"bytes"
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

func preflightCodes(t *testing.T, s *spec.Spec, report string) map[string]adapter.Level {
	t.Helper()
	got := map[string]adapter.Level{}
	for _, f := range parsePreflight([]byte(report)).findings(s, "h") {
		got[f.Code] = f.Level
	}
	return got
}

func TestPreflightOnAHealthyHost(t *testing.T) {
	got := preflightCodes(t, loadExample(t, "ethereum-node"), healthyHost)
	for code, level := range map[string]adapter.Level{
		"VPS_PREFLIGHT_COMPOSE_OK": adapter.Info,
		"VPS_PREFLIGHT_ARCH":       adapter.Info,
		"VPS_PREFLIGHT_DISK":       adapter.Info,
	} {
		if got[code] != level {
			t.Errorf("%s = %q, want %q (all: %v)", code, got[code], level, got)
		}
	}
	if _, ok := got["VPS_PREFLIGHT_PORT_IN_USE"]; ok {
		t.Error("no port is in use on this host")
	}
}

func TestPreflightFindsProblems(t *testing.T) {
	eth := loadExample(t, "ethereum-node")
	cases := map[string]struct {
		report string
		code   string
		level  adapter.Level
	}{
		"no docker": {"ANYSHIP version \nANYSHIP compose fail\n", "VPS_PREFLIGHT_DOCKER", adapter.Error},
		"compose rejects the file": {
			"ANYSHIP version 2.0.0\nANYSHIP compose fail\nANYSHIP disk / 999999999999\nANYSHIP running 0\nANYSHIP listen tcp 0.0.0.0:22\n",
			"VPS_PREFLIGHT_COMPOSE", adapter.Error,
		},
		"p2p port taken": {
			"ANYSHIP version 2.29.1\nANYSHIP compose ok\nANYSHIP disk / 999999999999\nANYSHIP running 0\nANYSHIP listen udp *:30303\n",
			"VPS_PREFLIGHT_PORT_IN_USE", adapter.Error,
		},
		"disk too small for 2.2 TB of volumes": {
			"ANYSHIP version 2.29.1\nANYSHIP compose ok\nANYSHIP disk /var/lib/docker 500000000\nANYSHIP running 0\nANYSHIP listen unavailable\n",
			"VPS_PREFLIGHT_DISK", adapter.Warning,
		},
		"arm64 host with images": {
			"ANYSHIP version 2.29.1\nANYSHIP compose ok\nANYSHIP arch aarch64\nANYSHIP disk / 999999999999\nANYSHIP running 0\n",
			"VPS_PREFLIGHT_ARCH", adapter.Warning,
		},
		"no ss": {
			"ANYSHIP version 2.29.1\nANYSHIP compose ok\nANYSHIP disk / 999999999999\nANYSHIP running 0\nANYSHIP listen unavailable\n",
			"VPS_PREFLIGHT_PORTS", adapter.Warning,
		},
	}
	for name, tc := range cases {
		if got := preflightCodes(t, eth, tc.report); got[tc.code] != tc.level {
			t.Errorf("%s: %s = %q, want %q (all: %v)", name, tc.code, got[tc.code], tc.level, got)
		}
	}
}

func TestPreflightSkipsPortsOnRedeploy(t *testing.T) {
	report := "ANYSHIP version 2.29.1\nANYSHIP compose ok\nANYSHIP disk / 999999999999\nANYSHIP running 2\nANYSHIP listen tcp 0.0.0.0:30303\n"
	got := preflightCodes(t, loadExample(t, "ethereum-node"), report)
	if _, inUse := got["VPS_PREFLIGHT_PORT_IN_USE"]; inUse || got["VPS_PREFLIGHT_PORTS"] != adapter.Info {
		t.Errorf("a running project's own ports aren't conflicts: %v", got)
	}
}

func TestApplyStopsBeforeUploadWhenPreflightFails(t *testing.T) {
	env, rec := newEnv(t, t.TempDir())
	rec.stdout[0] = "ANYSHIP version 2.29.1\nANYSHIP compose ok\nANYSHIP disk / 999999999999\nANYSHIP running 0\nANYSHIP listen tcp 0.0.0.0:30303\n"
	s := loadExample(t, "ethereum-node")
	result, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || len(rec.calls) != 1 || !slices.Contains(codesOf(result.Findings), "VPS_PREFLIGHT_PORT_IN_USE") {
		t.Errorf("result = %+v, calls = %d", result, len(rec.calls))
	}
}

func TestListenPort(t *testing.T) {
	for local, want := range map[string]string{
		"0.0.0.0:80": "80", "[::]:443": "443", "*:30303": "30303", "127.0.0.53%lo:53": "53", "garbage": "", "[::]:*": "",
	} {
		if got := listenPort(local); got != want {
			t.Errorf("listenPort(%q) = %q, want %q", local, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{2_200_000_000_000: "2.2 TB", 512_000_000_000: "512.0 GB", 5_000_000: "5.0 MB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestPreflightScriptRunsInAShell runs the real script with sh and the local
// docker CLI. Compose validation needs no daemon; the other facts depend on
// the machine, so only the report's shape is checked.
func TestPreflightScriptRunsInAShell(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not available")
	}
	env, _ := newEnv(t, t.TempDir())
	s := loadExample(t, "ethereum-node")
	p := plan(t, s, env)
	data := p.Data.(*planData)

	cmd := exec.Command("sh", "-c", preflightScript(data, "docker"))
	cmd.Stdin = bytes.NewReader(data.compose)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("script failed: %v\nstderr:\n%s", err, stderr.String())
	}
	report := parsePreflight(stdout.Bytes())
	if report.composeVersion == "" || !report.composeOK {
		t.Errorf("compose should validate the generated file: %+v\nstdout:\n%s\nstderr:\n%s", report, stdout.String(), stderr.String())
	}
	if report.arch == "" || !report.runningKnown {
		t.Errorf("report is missing facts: %+v\nstdout:\n%s", report, stdout.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if !strings.HasPrefix(line, preflightPrefix) {
			t.Errorf("unexpected stdout line %q", line)
		}
	}
}
