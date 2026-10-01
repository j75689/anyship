package vps

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

// call is one stubbed command invocation.
type call struct {
	name  string
	args  []string
	stdin []byte
}

func (c call) remote() string { return c.args[len(c.args)-1] }

type recorder struct {
	calls []call
	// fail makes the n-th call (0-based) fail; -1 never fails.
	fail int
}

func (r *recorder) exec(_ context.Context, opts adapter.ExecOptions, name string, args ...string) error {
	c := call{name: name, args: args}
	if opts.Stdin != nil {
		data, err := io.ReadAll(opts.Stdin)
		if err != nil {
			return err
		}
		c.stdin = data
	}
	r.calls = append(r.calls, c)
	if len(r.calls)-1 == r.fail {
		return errors.New("exit status 255")
	}
	return nil
}

func newEnv(t *testing.T, dir string) (*adapter.Env, *recorder) {
	t.Helper()
	rec := &recorder{fail: -1}
	return &adapter.Env{
		Dir:       dir,
		OutDir:    filepath.Join(dir, ".anyship", Name),
		Logf:      func(string, ...any) {},
		Exec:      rec.exec,
		LookupEnv: func(string) (string, bool) { return "", false },
	}, rec
}

func parse(t *testing.T, src string) *spec.Spec {
	t.Helper()
	s, err := spec.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func plan(t *testing.T, s *spec.Spec, env *adapter.Env) *adapter.Plan {
	t.Helper()
	p, err := New().Plan(context.Background(), s, env)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func codes(p *adapter.Plan, level adapter.Level) []string {
	var out []string
	for _, f := range p.Findings {
		if f.Level == level {
			out = append(out, f.Code)
		}
	}
	return out
}

func composeOf(t *testing.T, p *adapter.Plan) composeFile {
	t.Helper()
	if len(p.Files) != 1 {
		t.Fatalf("want 1 generated file, got %d (errors %v)", len(p.Files), codes(p, adapter.Error))
	}
	_, body, _ := bytes.Cut(p.Files[0].Contents, []byte("\n"))
	var c composeFile
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func loadExample(t *testing.T, name string) *spec.Spec {
	t.Helper()
	s, err := spec.Load(filepath.Join("..", "..", "examples", name, spec.Filename))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRendersEthereumNode(t *testing.T) {
	env, _ := newEnv(t, t.TempDir())
	p := plan(t, loadExample(t, "ethereum-node"), env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	c := composeOf(t, p)

	reth := c.Services["reth"]
	if want := []string{"30303:30303", "30303:30303/udp"}; !slices.Equal(reth.Ports, want) {
		t.Errorf("reth ports = %v, want %v (internal rpc/engine ports must not be published)", reth.Ports, want)
	}
	if !slices.Equal(reth.Entrypoint, []string{"reth"}) || reth.Command[0] != "node" || !slices.Contains(reth.Command, "/run/secrets/JWT_SECRET") {
		t.Errorf("reth entrypoint/command = %v %v", reth.Entrypoint, reth.Command)
	}
	if !slices.Equal(reth.Volumes, []string{"reth-data:/data"}) || !slices.Equal(reth.Secrets, []string{"JWT_SECRET"}) {
		t.Errorf("reth volumes/secrets = %v %v", reth.Volumes, reth.Secrets)
	}
	if reth.Restart != "unless-stopped" {
		t.Errorf("restart = %q", reth.Restart)
	}
	if lh := c.Services["lighthouse"]; !slices.Equal(lh.DependsOn, []string{"reth"}) {
		t.Errorf("lighthouse depends_on = %v", lh.DependsOn)
	}
	if c.Secrets["JWT_SECRET"].File != "./secrets/JWT_SECRET" || len(c.Volumes) != 2 {
		t.Errorf("top-level secrets/volumes = %v %v", c.Secrets, c.Volumes)
	}
	if !slices.Contains(codes(p, adapter.Info), "VPS_VOLUME_SIZE") {
		t.Errorf("want a volume size note, got %v", codes(p, adapter.Info))
	}
	if !slices.ContainsFunc(p.Actions, func(a adapter.Action) bool {
		return a.Kind == "secret" && a.Name == "JWT_SECRET" && strings.Contains(a.Detail, "generated on the host")
	}) {
		t.Errorf("actions = %+v", p.Actions)
	}
}

func TestRequiresAValidHost(t *testing.T) {
	env, _ := newEnv(t, t.TempDir())
	base := `"version": 1, "name": "app", "services": {"web": {"kind": "server", "image": "nginx"}}`
	for _, targets := range []string{
		``,
		`, "targets": {"vps": {}}`,
		`, "targets": {"vps": {"host": "-oProxyCommand=evil"}}`,
		`, "targets": {"vps": {"host": "a b"}}`,
		`, "targets": {"vps": {"host": "h", "dir": "~/apps"}}`,
		`, "targets": {"vps": {"host": "h", "hostname": "typo"}}`,
	} {
		p := plan(t, parse(t, `{`+base+targets+`}`), env)
		if got := codes(p, adapter.Error); !slices.Equal(got, []string{"VPS_BAD_OPTIONS"}) {
			t.Errorf("targets %q: errors = %v", targets, got)
		}
	}
}

func TestRefusesWhatItCannotRunYet(t *testing.T) {
	env, _ := newEnv(t, t.TempDir())
	cases := map[string]string{
		`"web": {"kind": "static", "build": {"output": "dist"}}`:                                                                         "VPS_STATIC_UNSUPPORTED",
		`"web": {"kind": "server", "start": "node server.js"}`:                                                                           "VPS_NEEDS_IMAGE",
		`"web": {"kind": "server", "image": "app", "start": "node a.js | tee log"}`:                                                      "VPS_BAD_START",
		`"web": {"kind": "server", "image": "app", "cron": [{"schedule": "* * * * *"}]}`:                                                 "VPS_CRON_UNSUPPORTED",
		`"web": {"kind": "server", "image": "app", "replicas": 2, "ports": [{"port": 80}]}`:                                              "VPS_REPLICAS_WITH_PORTS",
		`"web": {"kind": "server", "image": "app", "dockerfile": "Dockerfile"}`:                                                          "VPS_DOCKERFILE_MISSING",
		`"a": {"kind": "server", "image": "x", "ports": [{"port": 80}]}, "b": {"kind": "server", "image": "y", "ports": [{"port": 80}]}`: "VPS_PORT_CONFLICT",
	}
	for services, want := range cases {
		p := plan(t, parse(t, `{"version": 1, "name": "app", "services": {`+services+`}, "targets": {"vps": {"host": "h"}}}`), env)
		if got := codes(p, adapter.Error); !slices.Contains(got, want) {
			t.Errorf("services %s: errors = %v, want %s", services, got, want)
		}
		if len(p.Files) != 0 {
			t.Errorf("services %s: a refused plan must not generate files", services)
		}
	}

	resources := plan(t, parse(t, `{"version": 1, "name": "app",
		"services": {"web": {"kind": "server", "image": "app", "uses": ["db", "cache"]}},
		"resources": {"db": {"type": "postgres"}, "cache": {"type": "redis", "external": true}},
		"targets": {"vps": {"host": "h"}}}`), env)
	if got := codes(resources, adapter.Error); !slices.Equal(got, []string{"VPS_RESOURCE_UNSUPPORTED"}) {
		t.Errorf("resource errors = %v", got)
	}
	if !slices.Contains(codes(resources, adapter.Info), "VPS_EXTERNAL_RESOURCE") {
		t.Errorf("external resources should be allowed with a note")
	}
}

func TestDockerfileServicesUploadTheirContext(t *testing.T) {
	dir := t.TempDir()
	for name, contents := range map[string]string{
		"api/Dockerfile":            "FROM alpine\n",
		"api/main.go":               "package main\n",
		"api/node_modules/x/a.js":   "ignored",
		"api/.git/HEAD":             "ignored",
		"api/.anyship/vps/old.yaml": "ignored",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := parse(t, `{"version": 1, "name": "shop",
		"services": {"api": {"kind": "server", "path": "api", "dockerfile": "Dockerfile", "ports": [{"port": 8080}], "secrets": ["API_KEY", "SESSION_KEY"]}},
		"secrets": {"API_KEY": {}, "SESSION_KEY": {"generate": "hex32"}},
		"targets": {"vps": {"host": "deploy@203.0.113.10", "port": 2222, "dir": "apps/shop", "sudo": true}}}`)
	env, rec := newEnv(t, dir)
	env.LookupEnv = func(key string) (string, bool) {
		if key == "API_KEY" {
			return "s3cret", true
		}
		return "", false
	}

	p := plan(t, s, env)
	if build := composeOf(t, p).Services["api"].Build; build == nil || build.Context != "src/api" || build.Dockerfile != "Dockerfile" {
		t.Fatalf("build = %+v", build)
	}
	result, err := New().Apply(context.Background(), p, s, env)
	if err != nil || !result.OK {
		t.Fatalf("apply = %+v, %v", result, err)
	}

	if len(rec.calls) != 3 {
		t.Fatalf("want preflight, upload and deploy, got %d calls", len(rec.calls))
	}
	wantPrefix := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-p", "2222", "--", "deploy@203.0.113.10"}
	for _, c := range rec.calls {
		if c.name != "ssh" || len(c.args) != len(wantPrefix)+1 || !slices.Equal(c.args[:len(wantPrefix)], wantPrefix) {
			t.Errorf("ssh args = %q, want %q followed by the remote command", c.args, wantPrefix)
		}
	}
	if got := rec.calls[0].remote(); got != "sudo -n docker compose version" {
		t.Errorf("preflight = %q", got)
	}
	if got := rec.calls[1].remote(); got != "mkdir -p apps/shop && rm -rf apps/shop/src && tar -xf - -C apps/shop" {
		t.Errorf("upload = %q", got)
	}

	files := untar(t, rec.calls[1].stdin)
	if files["secrets/API_KEY"] != "s3cret" {
		t.Errorf("API_KEY should be uploaded from the environment, got %q", files["secrets/API_KEY"])
	}
	if _, ok := files["secrets/SESSION_KEY"]; ok {
		t.Error("generated secrets must be created on the host, not uploaded")
	}
	if files["src/api/main.go"] != "package main\n" || !strings.Contains(files["compose.yaml"], `"name": "shop"`) {
		t.Errorf("bundle is missing the build context or compose file: %v", slices.Sorted(maps.Keys(files)))
	}
	for name := range files {
		if strings.Contains(name, "node_modules") || strings.Contains(name, ".git/") || strings.Contains(name, ".anyship") {
			t.Errorf("bundle should not contain %s", name)
		}
	}

	script := rec.calls[2].remote()
	for _, want := range []string{
		"cd apps/shop",
		"if [ ! -s secrets/SESSION_KEY ]; then head -c 32 /dev/urandom",
		"if [ ! -s secrets/API_KEY ]; then echo 'anyship: secret API_KEY has no value",
		"sudo -n docker compose -p shop -f compose.yaml up -d --build --remove-orphans",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("deploy script missing %q:\n%s", want, script)
		}
	}
}

func TestApplyDryRunOnlyChecksTheHost(t *testing.T) {
	env, rec := newEnv(t, t.TempDir())
	env.DryRun = true
	s := loadExample(t, "ethereum-node")
	result, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil || !result.OK {
		t.Fatalf("apply = %+v, %v", result, err)
	}
	if len(rec.calls) != 1 || rec.calls[0].remote() != "docker compose version" {
		t.Errorf("dry run calls = %+v", rec.calls)
	}
}

func TestApplyStopsWhenTheHostIsUnreachable(t *testing.T) {
	env, rec := newEnv(t, t.TempDir())
	rec.fail = 0
	s := loadExample(t, "ethereum-node")
	result, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || len(rec.calls) != 1 || !strings.Contains(result.Messages[0], "Cannot run Docker Compose") {
		t.Errorf("result = %+v, calls = %d", result, len(rec.calls))
	}
}

func TestApplyReportsAFailedUpload(t *testing.T) {
	env, rec := newEnv(t, t.TempDir())
	rec.fail = 1
	s := loadExample(t, "ethereum-node")
	result, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || len(rec.calls) != 2 || !strings.Contains(result.Messages[0], "Upload to") {
		t.Errorf("result = %+v, calls = %d", result, len(rec.calls))
	}
}

// TestComposeFileIsValid asks Docker Compose itself to validate the rendered
// file. It needs only the docker CLI with the compose plugin, not a daemon.
func TestComposeFileIsValid(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not available")
	}
	env, _ := newEnv(t, t.TempDir())
	p := plan(t, loadExample(t, "ethereum-node"), env)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets", "JWT_SECRET"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), p.Files[0].Contents, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("docker", "compose", "-f", "compose.yaml", "config", "--quiet")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker compose config: %v\n%s", err, out)
	}
}

func untar(t *testing.T, data []byte) map[string]string {
	t.Helper()
	files := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name] = string(body)
	}
}
