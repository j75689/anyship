package vps

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
)

// scripted stubs Exec with fixed output and exit status, like a remote host.
type scripted struct {
	calls  []call
	stdout string
	stderr string
	exit   int
}

type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitError) ExitCode() int { return int(e) }

func (s *scripted) exec(_ context.Context, opts adapter.ExecOptions, name string, args ...string) error {
	s.calls = append(s.calls, call{name: name, args: args})
	if opts.Stdout != nil {
		_, _ = io.WriteString(opts.Stdout, s.stdout)
	}
	if opts.Stderr != nil {
		_, _ = io.WriteString(opts.Stderr, s.stderr)
	}
	if s.exit != 0 {
		return exitError(s.exit)
	}
	return nil
}

func scriptedEnv(t *testing.T, sc *scripted) *adapter.Env {
	env, _ := newEnv(t, t.TempDir())
	env.Exec = sc.exec
	return env
}

const psOutput = `{"Name":"eth-mainnet-reth-1","Service":"reth","State":"running","Health":"healthy","Status":"Up 2 hours (healthy)","Image":"ghcr.io/paradigmxyz/reth:v1.0.0","Created":1791417600,"Publishers":[{"URL":"0.0.0.0","TargetPort":30303,"PublishedPort":30303,"Protocol":"tcp"},{"URL":"::","TargetPort":30303,"PublishedPort":30303,"Protocol":"tcp"},{"URL":"0.0.0.0","TargetPort":30303,"PublishedPort":30303,"Protocol":"udp"},{"URL":"","TargetPort":8545,"PublishedPort":0,"Protocol":"tcp"}]}
{"Name":"eth-mainnet-lighthouse-1","Service":"lighthouse","State":"exited","Health":"","Status":"Exited (1) 3 minutes ago","Image":"sigp/lighthouse:v5","Created":1791421200,"Publishers":[]}
---anyship-inspect---
reth 0
lighthouse 4
`

func TestStatusMergesContainersPerService(t *testing.T) {
	sc := &scripted{stdout: psOutput}
	st, err := New().Status(context.Background(), loadExample(t, "ethereum-node"), scriptedEnv(t, sc))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Deployed || st.Location != "deploy@203.0.113.10:anyship/eth-mainnet" {
		t.Errorf("status = %+v", st)
	}
	none, four := 0, 4
	want := []adapter.ServiceStatus{
		{Name: "lighthouse", State: "exited", Running: 0, Desired: 1, Detail: "Exited (1) 3 minutes ago", Since: "2026-10-08T01:00:00Z", Restarts: &four, Image: "sigp/lighthouse:v5"},
		{Name: "reth", State: "running", Health: "healthy", Running: 1, Desired: 1, Ports: []string{"30303/tcp", "30303/udp"}, Detail: "Up 2 hours (healthy)", URL: "203.0.113.10:30303", Since: "2026-10-08T00:00:00Z", Restarts: &none, Image: "ghcr.io/paradigmxyz/reth:v1.0.0"},
	}
	for i := range want {
		got := st.Services[i]
		if got.Restarts == nil || want[i].Restarts == nil || *got.Restarts != *want[i].Restarts {
			t.Errorf("%s restarts = %v, want %v", got.Name, got.Restarts, want[i].Restarts)
		}
		got.Restarts, want[i].Restarts = nil, nil
		if fmt.Sprint(got) != fmt.Sprint(want[i]) {
			t.Errorf("%s =\n %+v\nwant\n %+v", got.Name, got, want[i])
		}
	}
	if st.Healthy() {
		t.Error("a deployment with an exited service is not healthy")
	}
	remote := sc.calls[0].remote()
	if !strings.HasPrefix(remote, "cd anyship/eth-mainnet 2>/dev/null && [ -f compose.yaml ] || exit 3; ") ||
		!strings.Contains(remote, "docker compose -p eth-mainnet -f compose.yaml ps --all --format json; echo ---anyship-inspect---;") ||
		!strings.Contains(remote, `docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}} {{.RestartCount}}' $ids`) {
		t.Errorf("remote = %q", remote)
	}
}

func TestStatusReadsArrayOutputFromOlderCompose(t *testing.T) {
	sc := &scripted{stdout: `[{"Service":"reth","State":"running"},{"Service":"lighthouse","State":"running"}]`}
	st, err := New().Status(context.Background(), loadExample(t, "ethereum-node"), scriptedEnv(t, sc))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Healthy() {
		t.Errorf("status = %+v", st)
	}
}

func TestStatusWhenNothingIsDeployed(t *testing.T) {
	st, err := New().Status(context.Background(), loadExample(t, "ethereum-node"), scriptedEnv(t, &scripted{exit: notDeployedExit}))
	if err != nil {
		t.Fatal(err)
	}
	if st.Deployed || len(st.Services) != 2 || st.Services[0].State != "missing" {
		t.Errorf("status = %+v", st)
	}

	_, err = New().Status(context.Background(), loadExample(t, "ethereum-node"), scriptedEnv(t, &scripted{exit: 255}))
	if err == nil || !strings.Contains(err.Error(), "reading status from deploy@203.0.113.10 failed") {
		t.Errorf("an ssh failure should be an error, got %v", err)
	}
}

// A failed status has to say why: scripts and agents see the error, not the
// terminal ssh wrote to.
func TestStatusFailureKeepsTheCause(t *testing.T) {
	for name, tc := range map[string]struct {
		sc   scripted
		want string
	}{
		"unreachable host": {
			sc: scripted{exit: 255, stderr: "ssh: connect to host 203.0.113.10 port 22: Connection refused\r\n"},
			want: "reading status from deploy@203.0.113.10 failed (exit status 255): " +
				"ssh: connect to host 203.0.113.10 port 22: Connection refused. " +
				"Check that `ssh deploy@203.0.113.10` works without a password prompt.",
		},
		"ssh says nothing": {
			sc: scripted{exit: 255},
			want: "reading status from deploy@203.0.113.10 failed (exit status 255). " +
				"Check that `ssh deploy@203.0.113.10` works without a password prompt.",
		},
		// The connection is fine here, so there is nothing to check about ssh.
		"docker fails on the host": {
			sc: scripted{exit: 1, stderr: "Warning: Permanently added '203.0.113.10' (ED25519) to the list of known hosts.\n" +
				"permission denied while trying to connect to the Docker daemon socket\n"},
			want: "reading status from deploy@203.0.113.10 failed (exit status 1): " +
				"permission denied while trying to connect to the Docker daemon socket",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New().Status(context.Background(), loadExample(t, "ethereum-node"), scriptedEnv(t, &tc.sc))
			if err == nil || err.Error() != tc.want {
				t.Errorf("error = %v\nwant    %s", err, tc.want)
			}
			if code, ok := adapter.ExitCode(err); !ok || code != tc.sc.exit {
				t.Errorf("the exit status is lost: %d %v", code, ok)
			}
		})
	}
}

func TestSSHCommandMatchesTheOptions(t *testing.T) {
	for want, o := range map[string]Options{
		"ssh deploy@203.0.113.10":                              {Host: "deploy@203.0.113.10"},
		"ssh -p 2222 -i ~/.ssh/id_ed25519 deploy@203.0.113.10": {Host: "deploy@203.0.113.10", Port: 2222, IdentityFile: "~/.ssh/id_ed25519"},
		"ssh -i ~/'.ssh/deploy key' deploy@203.0.113.10":       {Host: "deploy@203.0.113.10", IdentityFile: "~/.ssh/deploy key"},
		"ssh -i '/keys/deploy key' deploy@203.0.113.10":        {Host: "deploy@203.0.113.10", IdentityFile: "/keys/deploy key"},
	} {
		if got := sshCommand(o); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestDestroyKeepsDataUnlessAsked(t *testing.T) {
	s := loadExample(t, "ethereum-node")

	sc := &scripted{}
	result, err := New().Destroy(context.Background(), s, scriptedEnv(t, sc), adapter.DestroyOptions{})
	if err != nil || !result.OK || !strings.Contains(result.Messages[0], "volumes and secrets were kept") {
		t.Fatalf("result = %+v, %v", result, err)
	}
	script := sc.calls[0].remote()
	if !strings.Contains(script, "docker compose -p eth-mainnet -f compose.yaml down --remove-orphans\n") || strings.Contains(script, "--volumes") || strings.Contains(script, "rm -rf") {
		t.Errorf("script:\n%s", script)
	}

	sc = &scripted{}
	result, err = New().Destroy(context.Background(), s, scriptedEnv(t, sc), adapter.DestroyOptions{Volumes: true})
	if err != nil || !result.OK {
		t.Fatalf("result = %+v, %v", result, err)
	}
	script = sc.calls[0].remote()
	for _, want := range []string{"set -eu\n", "down --remove-orphans --volumes\n", "cd && rm -rf anyship/eth-mainnet\n"} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
}

func TestDestroyWhenNothingIsDeployed(t *testing.T) {
	result, err := New().Destroy(context.Background(), loadExample(t, "ethereum-node"), scriptedEnv(t, &scripted{exit: notDeployedExit}), adapter.DestroyOptions{})
	if err != nil || !result.OK || !strings.HasPrefix(result.Messages[0], "Nothing to remove") {
		t.Errorf("result = %+v, %v", result, err)
	}
}

func TestDestroySummary(t *testing.T) {
	s := loadExample(t, "ethereum-node")
	keep, err := New().DestroySummary(s, adapter.DestroyOptions{})
	if err != nil || len(keep) != 2 || !strings.Contains(keep[1], "Keep the volumes lighthouse-data, reth-data") {
		t.Errorf("summary = %q, %v", keep, err)
	}
	drop, _ := New().DestroySummary(s, adapter.DestroyOptions{Volumes: true})
	if !strings.Contains(drop[1], "DELETE the volumes lighthouse-data, reth-data") {
		t.Errorf("summary = %q", drop)
	}
}

func TestDirMustBeSafeToDelete(t *testing.T) {
	for dir, ok := range map[string]bool{
		"apps/shop": true, "/srv/shop": true, "/srv/shop/": true,
		".": false, "./": false, "/": false, "/srv": false, "..": false, "../shop": false, "apps/../..": false,
	} {
		_, err := decodeOptions([]byte(fmt.Sprintf(`{"host": "h", "dir": %q}`, dir)))
		if (err == nil) != ok {
			t.Errorf("dir %q: err = %v, want ok=%v", dir, err, ok)
		}
	}
}
