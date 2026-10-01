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

const psOutput = `{"Name":"eth-mainnet-reth-1","Service":"reth","State":"running","Health":"healthy","Status":"Up 2 hours (healthy)","Publishers":[{"URL":"0.0.0.0","TargetPort":30303,"PublishedPort":30303,"Protocol":"tcp"},{"URL":"::","TargetPort":30303,"PublishedPort":30303,"Protocol":"tcp"},{"URL":"0.0.0.0","TargetPort":30303,"PublishedPort":30303,"Protocol":"udp"},{"URL":"","TargetPort":8545,"PublishedPort":0,"Protocol":"tcp"}]}
{"Name":"eth-mainnet-lighthouse-1","Service":"lighthouse","State":"exited","Health":"","Status":"Exited (1) 3 minutes ago","Publishers":[]}
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
	want := []adapter.ServiceStatus{
		{Name: "lighthouse", State: "exited", Running: 0, Desired: 1, Detail: "Exited (1) 3 minutes ago"},
		{Name: "reth", State: "running", Health: "healthy", Running: 1, Desired: 1, Ports: []string{"30303/tcp", "30303/udp"}, Detail: "Up 2 hours (healthy)"},
	}
	if fmt.Sprint(st.Services) != fmt.Sprint(want) {
		t.Errorf("services =\n %+v\nwant\n %+v", st.Services, want)
	}
	if st.Healthy() {
		t.Error("a deployment with an exited service is not healthy")
	}
	remote := sc.calls[0].remote()
	if !strings.HasPrefix(remote, "cd anyship/eth-mainnet 2>/dev/null && [ -f compose.yaml ] || exit 3; ") ||
		!strings.HasSuffix(remote, "docker compose -p eth-mainnet -f compose.yaml ps --all --format json") {
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
