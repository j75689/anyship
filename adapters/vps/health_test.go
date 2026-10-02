package vps

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/dockerfile"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// healthSpec exercises every way a health check can reach compose.yaml: a path
// on a public port, a path on an internal port, a path needing quoting, an
// explicit command, and no health check at all.
const healthSpec = `{"name": "shop",
	"services": {
		"web": {"kind": "server", "image": "shop/web:1", "ports": [{"port": 8080}],
			"healthCheck": {"path": "/healthz"}},
		"api": {"kind": "server", "image": "shop/api:1", "ports": [{"port": 9000, "exposure": "internal"}],
			"healthCheck": {"path": "ready?deep=1"}},
		"odd": {"kind": "server", "image": "shop/odd:1", "ports": [{"port": 7000}],
			"healthCheck": {"path": "/it's/fine"}},
		"queue": {"kind": "worker", "image": "shop/queue:1",
			"healthCheck": {"command": "test -f /tmp/alive"}},
		"cache": {"kind": "worker", "image": "redis:7"}
	},
	"targets": {"vps": {"host": "h"}}}`

// TestComposeHealthchecksAreGolden pins the rendered compose.yaml, probe shell
// and all, so a change to the generated health checks has to be deliberate.
func TestComposeHealthchecksAreGolden(t *testing.T) {
	env, _ := newEnv(t, t.TempDir())
	p := plan(t, parse(t, healthSpec), env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	i := slices.IndexFunc(p.Files, func(f adapter.File) bool { return filepath.Base(f.Path) == "compose.yaml" })
	if i < 0 {
		t.Fatal("plan has no compose.yaml")
	}
	golden := filepath.Join("testdata", "healthcheck.compose.yaml")
	if *update {
		if err := os.WriteFile(golden, p.Files[i].Contents, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v; run `go test ./adapters/vps -update` to create it", err)
	}
	if !bytes.Equal(want, p.Files[i].Contents) {
		t.Errorf("compose.yaml differs from %s; run `go test ./adapters/vps -update` if the change is intended.\ngot:\n%s", golden, p.Files[i].Contents)
	}
}

func TestHealthCheckFindings(t *testing.T) {
	env, _ := newEnv(t, t.TempDir())
	cases := []struct {
		service string
		code    string
		level   adapter.Level
	}{
		{`"web": {"kind": "server", "image": "app", "ports": [{"port": 80}], "healthCheck": {"path": "/up"}}`, "VPS_HEALTH_PATH", adapter.Info},
		{`"web": {"kind": "worker", "image": "app", "healthCheck": {"path": "/up"}}`, "VPS_HEALTH_PATH_NO_PORT", adapter.Warning},
		{`"web": {"kind": "server", "image": "app", "ports": [{"port": 80, "protocol": "tcp"}], "healthCheck": {"path": "/up"}}`, "VPS_HEALTH_PATH_NO_PORT", adapter.Warning},
		{`"web": {"kind": "server", "image": "app", "ports": [{"port": 80}], "healthCheck": {"path": "/a b"}}`, "VPS_HEALTH_BAD_PATH", adapter.Error},
	}
	for _, c := range cases {
		p := plan(t, parse(t, `{"name": "app", "services": {`+c.service+`}, "targets": {"vps": {"host": "h"}}}`), env)
		if got := codes(p, c.level); !slices.Contains(got, c.code) {
			t.Errorf("%s: %s findings = %v, want %s", c.service, c.level, got, c.code)
		}
	}

	// The old "path is ignored" finding must not come back.
	p := plan(t, parse(t, `{"name": "app",
		"services": {"web": {"kind": "server", "image": "app", "ports": [{"port": 80}], "healthCheck": {"path": "/up"}}},
		"targets": {"vps": {"host": "h"}}}`), env)
	for _, f := range p.Findings {
		if f.Code == "VPS_HEALTH_PATH_IGNORED" {
			t.Error("healthCheck.path is honoured now, so nothing should report it as ignored")
		}
	}
}

func TestHTTPPortPicksWhatTheProbeCanReach(t *testing.T) {
	cases := []struct {
		src  string
		want int
		ok   bool
	}{
		{`{"kind": "static"}`, dockerfile.StaticPort, true},
		{`{"kind": "static", "ports": [{"port": 8080}]}`, dockerfile.StaticPort, true},
		{`{"kind": "server", "image": "a", "ports": [{"port": 3000}]}`, 3000, true},
		{`{"kind": "server", "image": "a", "ports": [{"port": 5000, "exposure": "internal"}]}`, 5000, true},
		{`{"kind": "server", "image": "a", "ports": [{"port": 53, "protocol": "udp"}, {"port": 8080}]}`, 8080, true},
		{`{"kind": "worker", "image": "a"}`, 0, false},
		{`{"kind": "server", "image": "a", "ports": [{"port": 6379, "protocol": "tcp"}]}`, 0, false},
	}
	for _, c := range cases {
		s := parse(t, `{"name": "app", "services": {"x": `+c.src+`}, "targets": {"vps": {"host": "h"}}}`)
		got, ok := httpPort(s.Services["x"])
		if got != c.want || ok != c.ok {
			t.Errorf("httpPort(%s) = %d, %v; want %d, %v", c.src, got, ok, c.want, c.ok)
		}
	}
}

func TestHealthURLAddsTheLeadingSlash(t *testing.T) {
	for path, want := range map[string]string{
		"/health": "http://127.0.0.1:8080/health",
		"health":  "http://127.0.0.1:8080/health",
		"":        "http://127.0.0.1:8080/",
	} {
		if got := healthURL(8080, path); got != want {
			t.Errorf("healthURL(8080, %q) = %q, want %q", path, got, want)
		}
	}
}

// TestHealthProbeRunsInAShell checks the generated probe against real wget and
// curl: it must pass on a path that answers, fail on one that doesn't, and say
// so when neither tool is on PATH, which is what an image without them looks
// like from inside the container.
func TestHealthProbeRunsInAShell(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/up" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintln(w, "ok")
	}))
	defer srv.Close()
	url, _ := strings.CutPrefix(srv.URL, "http://")

	run := func(path, pathEnv string) (string, error) {
		cmd := exec.Command("sh", "-c", healthProbe("http://"+url+path))
		cmd.Env = append(os.Environ(), "PATH="+pathEnv)
		var combined bytes.Buffer
		cmd.Stdout, cmd.Stderr = &combined, &combined
		err := cmd.Run()
		t.Logf("probe %q with PATH=%q: %v\n%s", path, pathEnv, err, combined.String())
		return combined.String(), err
	}
	if _, err := run("/up", os.Getenv("PATH")); err != nil {
		t.Errorf("the probe should pass on a path that answers: %v", err)
	}
	if _, err := run("/missing", os.Getenv("PATH")); err == nil {
		t.Error("the probe should fail on a 404")
	}
	// Without wget or curl the probe passes, because a check anyship cannot
	// run is no reason to call a working service broken -- but it prints why,
	// and Apply turns that into a warning.
	out, err := run("/up", "")
	if err != nil {
		t.Errorf("without wget or curl the probe should pass with a note, not fail: %v", err)
	}
	if !strings.Contains(out, noHTTPClient) {
		t.Errorf("a probe without wget or curl must explain itself, got %q", out)
	}
}

func TestParseHealthKeepsTheWorstContainerPerService(t *testing.T) {
	r := parseHealth([]byte(`ANYSHIP health web running healthy
ANYSHIP health web running unhealthy
ANYSHIP health api exited unhealthy
ANYSHIP health cache running none
ANYSHIP noprobe cache
not an anyship line
`))
	if !maps.Equal(r.health, map[string]string{"web": "unhealthy", "api": "unhealthy", "cache": "none"}) {
		t.Errorf("health = %v", r.health)
	}
	if !maps.Equal(r.status, map[string]string{"web": "running", "api": "exited", "cache": "running"}) {
		t.Errorf("status = %v", r.status)
	}
	if !r.noProbe["cache"] || r.noProbe["web"] {
		t.Errorf("noProbe = %v", r.noProbe)
	}
}

func TestHealthFindings(t *testing.T) {
	cases := []struct {
		report string
		code   string
		level  adapter.Level
	}{
		{"ANYSHIP health web running healthy\n", "VPS_HEALTH_OK", adapter.Info},
		{"ANYSHIP health web running unhealthy\n", "VPS_HEALTH_UNHEALTHY", adapter.Error},
		{"ANYSHIP health web running starting\n", "VPS_HEALTH_TIMEOUT", adapter.Error},
		{"ANYSHIP health web exited unhealthy\n", "VPS_HEALTH_NOT_RUNNING", adapter.Error},
		{"", "VPS_HEALTH_NO_CONTAINER", adapter.Error},
		{"ANYSHIP health web running none\n", "VPS_HEALTH_NOT_CONFIGURED", adapter.Warning},
		{"ANYSHIP health web running healthy\nANYSHIP noprobe web\n", "VPS_HEALTH_NO_HTTP_CLIENT", adapter.Warning},
	}
	for _, c := range cases {
		got := parseHealth([]byte(c.report)).findings([]string{"web"}, "h")
		if len(got) != 1 || got[0].Code != c.code || got[0].Level != c.level {
			t.Errorf("report %q gave %v, want one %s %s", c.report, got, c.level, c.code)
			continue
		}
		if got[0].Service != "web" {
			t.Errorf("report %q: finding has no service", c.report)
		}
		if c.level != adapter.Info && got[0].Hint == "" {
			t.Errorf("report %q: %s should carry a hint", c.report, c.code)
		}
	}

	// Services without a health check are not judged either way.
	if got := parseHealth([]byte("ANYSHIP health cache running none\n")).findings(nil, "h"); len(got) != 0 {
		t.Errorf("findings for no services = %v", got)
	}
}

func TestApplyWaitsForHealthAndFailsWhenItNeverComes(t *testing.T) {
	healthy := "ANYSHIP health web running healthy\n"
	cases := map[string]struct {
		report string
		ok     bool
		code   string
	}{
		"healthy":   {healthy, true, "VPS_HEALTH_OK"},
		"unhealthy": {"ANYSHIP health web running unhealthy\n", false, "VPS_HEALTH_UNHEALTHY"},
		"timeout":   {"ANYSHIP health web running starting\n", false, "VPS_HEALTH_TIMEOUT"},
		"degraded":  {healthy + "ANYSHIP noprobe web\n", true, "VPS_HEALTH_NO_HTTP_CLIENT"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env, rec := newEnv(t, t.TempDir())
			rec.stdout[3] = c.report // 0 preflight, 1 upload, 2 compose up, 3 wait
			s := parse(t, `{"name": "app",
				"services": {"web": {"kind": "server", "image": "app", "ports": [{"port": 8080}], "healthCheck": {"path": "/up"}}},
				"targets": {"vps": {"host": "h"}}}`)
			result, err := New().Apply(context.Background(), plan(t, s, env), s, env)
			if err != nil {
				t.Fatal(err)
			}
			if len(rec.calls) != 4 {
				t.Fatalf("apply made %d ssh calls, want 4 (preflight, upload, up, wait)", len(rec.calls))
			}
			if !strings.Contains(rec.calls[3].remote(), "ANYSHIP health") {
				t.Errorf("the fourth call should be the health wait:\n%s", rec.calls[3].remote())
			}
			if result.OK != c.ok {
				t.Errorf("result = %+v, want OK=%v", result, c.ok)
			}
			if !slices.Contains(codesOf(result.Findings), c.code) {
				t.Errorf("findings = %v, want %s", codesOf(result.Findings), c.code)
			}
			if !c.ok && !strings.Contains(result.Messages[0], "never became healthy") {
				t.Errorf("message = %q", result.Messages[0])
			}
		})
	}
}

func TestApplySkipsTheWaitWithoutHealthChecks(t *testing.T) {
	env, rec := newEnv(t, t.TempDir())
	s := parse(t, `{"name": "app",
		"services": {"web": {"kind": "server", "image": "app", "ports": [{"port": 8080}]}},
		"targets": {"vps": {"host": "h"}}}`)
	result, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil || !result.OK {
		t.Fatalf("apply = %+v, %v", result, err)
	}
	if len(rec.calls) != 3 {
		t.Errorf("apply made %d ssh calls, want 3 (preflight, upload, up)", len(rec.calls))
	}
}

func TestApplyReportsAnUnreadableHealthState(t *testing.T) {
	env, rec := newEnv(t, t.TempDir())
	rec.fail = 3
	s := parse(t, `{"name": "app",
		"services": {"web": {"kind": "server", "image": "app", "ports": [{"port": 8080}], "healthCheck": {"path": "/up"}}},
		"targets": {"vps": {"host": "h"}}}`)
	result, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || !slices.Contains(codesOf(result.Findings), "VPS_HEALTH_UNKNOWN") {
		t.Errorf("result = %+v, findings = %v", result, codesOf(result.Findings))
	}
}

// runWaitScript runs the real wait script with sh against the stub docker, so
// the shell quoting, the polling loop and the report all get exercised without
// a Docker daemon. files are laid out for the stub: state/<id> is what inspect
// reports, flip/<id> replaces it after the first look, log/<id> is the health
// log and the log tail, and containers lists the ids.
func runWaitScript(t *testing.T, files map[string]string) (stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	files["deploy/compose.yaml"] = "# generated\n"
	files["bin/docker"] = stubDocker
	writeFiles(t, dir, files)
	stub := filepath.Join(dir, "bin")
	if err := os.Chmod(filepath.Join(stub, "docker"), 0o755); err != nil {
		t.Fatal(err)
	}

	data := &planData{project: "app", dir: filepath.Join(dir, "deploy")}
	cmd := exec.Command("sh", "-c", waitScript(data, "docker"))
	cmd.Env = append(os.Environ(), "PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB="+dir)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		t.Fatalf("script failed: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errs.String())
	}
	return out.String(), errs.String()
}

func TestWaitScriptReportsEveryContainer(t *testing.T) {
	stdout, stderr := runWaitScript(t, map[string]string{
		"state/c1":   "web running healthy\n",
		"state/c2":   "worker exited unhealthy\n",
		"log/c1":     "web said " + noHTTPClient + "\n",
		"log/c2":     "worker crashed: no such file\n",
		"containers": "c1\nc2\n",
	})

	r := parseHealth([]byte(stdout))
	if r.health["web"] != "healthy" || r.status["worker"] != "exited" {
		t.Errorf("report = %+v\nstdout:\n%s", r, stdout)
	}
	if !r.noProbe["web"] {
		t.Errorf("the probe note in web's health log should be reported:\nstdout:\n%s", stdout)
	}
	// The log tail of what is not healthy goes to stderr, where the user sees it.
	if !strings.Contains(stderr, "worker crashed: no such file") {
		t.Errorf("stderr should carry the unhealthy service's log tail:\n%s", stderr)
	}
	if strings.Contains(stderr, "web said") {
		t.Errorf("a healthy service needs no log tail:\n%s", stderr)
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if !strings.HasPrefix(line, reportPrefix) {
			t.Errorf("unexpected stdout line %q", line)
		}
	}
}

// TestWaitScriptWaitsWhileStarting makes the stub report "starting" once, so
// the script has to poll again before it reports.
func TestWaitScriptWaitsWhileStarting(t *testing.T) {
	stdout, _ := runWaitScript(t, map[string]string{
		"state/c1":   "web running starting\n",
		"flip/c1":    "web running healthy\n",
		"log/c1":     "listening on 8080\n",
		"containers": "c1\n",
	})
	if got := parseHealth([]byte(stdout)).health["web"]; got != "healthy" {
		t.Errorf("health = %q, want healthy; the script should wait out a starting container\nstdout:\n%s", got, stdout)
	}
}

func TestWaitScriptStopsWhenNothingIsDeployed(t *testing.T) {
	data := &planData{project: "app", dir: filepath.Join(t.TempDir(), "missing")}
	cmd := exec.Command("sh", "-c", waitScript(data, "docker"))
	err := cmd.Run()
	if code, ok := adapter.ExitCode(err); !ok || code != notDeployedExit {
		t.Errorf("exit = %v, want %d", err, notDeployedExit)
	}
}

// stubDocker answers the handful of docker calls waitScript makes, reading its
// answers from $STUB so the test decides what the host looks like.
const stubDocker = `#!/bin/sh
mode=
log=
for a in "$@"; do
  case "$a" in
    ps) mode=ps ;;
    inspect) mode=${mode:-inspect} ;;
    logs) mode=logs ;;
    *Health.Log*) log=1 ;;
  esac
  last=$a
done
case "$mode" in
  ps) cat "$STUB/containers" ;;
  logs) cat "$STUB/log/$last" ;;
  inspect)
    if [ -n "$log" ]; then
      cat "$STUB/log/$last"
    else
      cat "$STUB/state/$last"
      if [ -f "$STUB/flip/$last" ]; then mv "$STUB/flip/$last" "$STUB/state/$last"; fi
    fi ;;
  *) echo "stub docker: unexpected $*" >&2; exit 1 ;;
esac
exit 0
`
