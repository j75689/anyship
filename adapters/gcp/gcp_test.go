package gcp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/internal/spectest"
	"github.com/j75689/anyship/spec"
)

// call is one stubbed command invocation.
type call struct {
	name  string
	args  []string
	stdin string
}

func (c call) line() string { return c.name + " " + strings.Join(c.args, " ") }

// fakeCloud answers commands by prefix, like a project with everything enabled.
type fakeCloud struct {
	calls []call
	// out maps a command prefix to its stdout.
	out map[string]string
	// fail lists command prefixes that exit non-zero.
	fail []string
	// digest is what the fake buildx reports.
	digest string
}

func newFakeCloud() *fakeCloud {
	return &fakeCloud{
		out: map[string]string{
			"gcloud auth print-access-token":         "ya29.token",
			"gcloud services list":                   "run.googleapis.com\nartifactregistry.googleapis.com\nsecretmanager.googleapis.com",
			"gcloud artifacts repositories describe": "DOCKER",
			"gcloud run services describe shop-web":  "https://shop-web-abc.a.run.app",
			"gcloud run services list":               "[]",
		},
		digest: "sha256:" + strings.Repeat("a", 64),
	}
}

func (f *fakeCloud) exec(_ context.Context, opts adapter.ExecOptions, name string, args ...string) error {
	c := call{name: name, args: args}
	if opts.Stdin != nil {
		data, err := io.ReadAll(opts.Stdin)
		if err != nil {
			return err
		}
		c.stdin = string(data)
	}
	f.calls = append(f.calls, c)
	line := c.line()
	for _, prefix := range f.fail {
		if strings.HasPrefix(line, prefix) {
			return errors.New("exit status 1")
		}
	}
	if name == "docker" && len(args) > 1 && args[1] == "build" {
		i := slices.Index(args, "--metadata-file")
		if err := os.WriteFile(args[i+1], []byte(`{"containerimage.digest": "`+f.digest+`"}`), 0o644); err != nil {
			return err
		}
	}
	if opts.Stdout != nil {
		for prefix, out := range f.out {
			if strings.HasPrefix(line, prefix) {
				_, _ = io.WriteString(opts.Stdout, out)
			}
		}
	}
	return nil
}

func (f *fakeCloud) find(prefix string) *call {
	for i := range f.calls {
		if strings.HasPrefix(f.calls[i].line(), prefix) {
			return &f.calls[i]
		}
	}
	return nil
}

func newEnv(t *testing.T, dir string, env map[string]string) (*adapter.Env, *fakeCloud) {
	t.Helper()
	fc := newFakeCloud()
	return &adapter.Env{
		Dir:    dir,
		OutDir: filepath.Join(dir, ".anyship", Name),
		Logf:   func(string, ...any) {},
		Exec:   fc.exec,
		LookupEnv: func(key string) (string, bool) {
			v, ok := env[key]
			return v, ok
		},
	}, fc
}

func parse(t *testing.T, src string) *spec.Spec {
	t.Helper()
	s, err := spectest.Parse(src)
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

const target = `"targets": {"gcp": {"project": "my-project", "region": "us-central1", "repository": "apps"}}`

// nodeApp writes a minimal Node server that anyship can generate a Dockerfile for.
func nodeApp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name": "web", "scripts": {"start": "node server.js"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.js"), []byte("require('http').createServer().listen(process.env.PORT)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPlanImageService(t *testing.T) {
	s := parse(t, `{"name": "shop",
		"services": {"web": {"kind": "server", "image": "nginx:1.27", "ports": [{"port": 80}],
			"env": {"MODE": "prod", "HOSTS": "a,b"}, "secrets": ["API_KEY"], "replicas": 2}},
		"secrets": {"API_KEY": {}}, `+target+`}`)
	env, _ := newEnv(t, t.TempDir(), nil)
	p := plan(t, s, env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	for _, want := range []string{"GCP_SECRETS_AS_ENV", "GCP_REPLICAS"} {
		if !slices.Contains(codes(p, adapter.Info), want) {
			t.Errorf("missing info %s in %v", want, codes(p, adapter.Info))
		}
	}
	data := p.Data.(*planData)
	args := strings.Join(deployArgs(data, data.services[0], "nginx:1.27"), " ")
	for _, want := range []string{
		"run deploy shop-web --image nginx:1.27 --region us-central1 --project my-project --port 80",
		"--labels anyship-project=shop,anyship-service=web",
		"--ingress all --allow-unauthenticated",
		"--set-env-vars ^|^HOSTS=a,b|MODE=prod",
		"--set-secrets API_KEY=shop-API_KEY:latest",
		"--min-instances 2",
		"--quiet",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("deploy args %q lack %q", args, want)
		}
	}
}

func TestPlanRefusesUnsupported(t *testing.T) {
	s := parse(t, `{"name": "shop",
		"services": {
			"site": {"kind": "static", "path": "site"},
			"job": {"kind": "worker", "image": "busybox"},
			"db": {"kind": "server", "image": "postgres:16", "ports": [{"port": 5432, "protocol": "tcp"}],
				"volumes": [{"name": "data", "mountPath": "/var/lib/postgresql/data", "size": "10GB"}]},
			"api": {"kind": "server", "image": "api", "ports": [{"port": 80}, {"port": 81}], "uses": ["cache"]},
			"www": {"kind": "server", "image": "www", "ports": [{"port": 80}], "domains": ["shop.example.com"]}},
		"resources": {"cache": {"type": "redis"}}, `+target+`}`)
	env, _ := newEnv(t, t.TempDir(), nil)
	errs := codes(plan(t, s, env), adapter.Error)
	for _, want := range []string{"GCP_STATIC", "GCP_WORKER", "GCP_VOLUMES", "GCP_NON_HTTP_PORT", "GCP_MULTIPLE_PORTS", "GCP_RESOURCE", "GCP_DOMAIN_UNSUPPORTED"} {
		if !slices.Contains(errs, want) {
			t.Errorf("missing error %s in %v", want, errs)
		}
	}
}

func TestPlanOptions(t *testing.T) {
	for name, targets := range map[string]string{
		"missing":       `{}`,
		"bad project":   `{"gcp": {"project": "X", "region": "us-central1"}}`,
		"bad region":    `{"gcp": {"project": "my-project", "region": "central"}}`,
		"unknown field": `{"gcp": {"project": "my-project", "region": "us-central1", "zone": "a"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}}, "targets": `+targets+`}`)
			env, _ := newEnv(t, t.TempDir(), nil)
			if errs := codes(plan(t, s, env), adapter.Error); !slices.Equal(errs, []string{"GCP_BAD_OPTIONS"}) {
				t.Errorf("errors = %v", errs)
			}
		})
	}
}

func TestPlanBuildNeedsRepository(t *testing.T) {
	dir := nodeApp(t)
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "start": "node server.js"}},
		"targets": {"gcp": {"project": "my-project", "region": "us-central1"}}}`)
	env, _ := newEnv(t, dir, nil)
	if errs := codes(plan(t, s, env), adapter.Error); !slices.Contains(errs, "GCP_NO_REPOSITORY") {
		t.Errorf("errors = %v", errs)
	}
}

func TestDeployOrderFollowsDependsOn(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {
		"a": {"kind": "server", "image": "x", "dependsOn": ["b"]},
		"b": {"kind": "server", "image": "x", "dependsOn": ["c"]},
		"c": {"kind": "server", "image": "x"}}, `+target+`}`)
	if got := s.DeployOrder(); !slices.Equal(got, []string{"c", "b", "a"}) {
		t.Errorf("order = %v", got)
	}
}

func TestApplyBuildsAndDeploysByDigest(t *testing.T) {
	dir := nodeApp(t)
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "start": "node server.js", "secrets": ["SESSION"]}},
		"secrets": {"SESSION": {"generate": "hex32"}}, `+target+`}`)
	env, fc := newEnv(t, dir, nil)
	fc.fail = []string{"gcloud secrets describe"}
	p := plan(t, s, env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Fatalf("plan errors: %v", errs)
	}
	res, err := New().Apply(context.Background(), p, s, env)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("apply failed: %v %v", res.Messages, res.Findings)
	}

	create := fc.find("gcloud secrets create shop-SESSION")
	if create == nil || len(create.stdin) != 64 {
		t.Fatalf("generated secret not created from stdin: %+v", create)
	}
	if strings.Contains(create.line(), create.stdin) {
		t.Error("secret value leaked onto the command line")
	}
	login := fc.find("docker login --username oauth2accesstoken --password-stdin https://us-central1-docker.pkg.dev")
	if login == nil || login.stdin != "ya29.token" {
		t.Fatalf("docker login = %+v", login)
	}
	if build := fc.find("docker buildx build --platform linux/amd64"); build == nil ||
		!slices.Contains(build.args, "us-central1-docker.pkg.dev/my-project/apps/shop-web:latest") {
		t.Fatalf("build = %+v", build)
	}
	ref := "us-central1-docker.pkg.dev/my-project/apps/shop-web@" + fc.digest
	if fc.find("gcloud run deploy shop-web --image "+ref) == nil {
		t.Errorf("not deployed by digest; calls: %v", fc.calls)
	}
	if !slices.Contains(res.Messages, "web: https://shop-web-abc.a.run.app") {
		t.Errorf("messages = %v", res.Messages)
	}
	if _, err := os.Stat(filepath.Join(env.OutDir, "web.Dockerfile.dockerignore")); err != nil {
		t.Errorf("generated ignore file not written: %v", err)
	}
}

func TestApplySecretFromEnvAddsVersion(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx", "secrets": ["TOKEN"]}},
		"secrets": {"TOKEN": {}}, `+target+`}`)
	env, fc := newEnv(t, t.TempDir(), map[string]string{"TOKEN": "s3cret"})
	res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil || !res.OK {
		t.Fatalf("apply: %v %+v", err, res)
	}
	if add := fc.find("gcloud secrets versions add shop-TOKEN"); add == nil || add.stdin != "s3cret" {
		t.Errorf("versions add = %+v", add)
	}
	if fc.find("docker") != nil {
		t.Error("image service should not touch docker")
	}
}

func TestApplyMissingSecretFails(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx", "secrets": ["TOKEN"]}},
		"secrets": {"TOKEN": {}}, `+target+`}`)
	env, fc := newEnv(t, t.TempDir(), nil)
	fc.fail = []string{"gcloud secrets describe"}
	res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil || res.OK {
		t.Fatalf("apply should fail: %v %+v", err, res)
	}
	if fc.find("gcloud run deploy") != nil {
		t.Error("deployed despite the missing secret")
	}
}

func TestApplyPreflight(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}}, `+target+`}`)

	t.Run("api disabled", func(t *testing.T) {
		env, fc := newEnv(t, t.TempDir(), nil)
		fc.out["gcloud services list"] = "compute.googleapis.com"
		res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
		if err != nil || res.OK {
			t.Fatalf("apply should fail: %v %+v", err, res)
		}
		if res.Findings[0].Code != "GCP_PREFLIGHT_API" || !strings.Contains(res.Findings[0].Hint, "gcloud services enable run.googleapis.com") {
			t.Errorf("findings = %+v", res.Findings)
		}
		if fc.find("gcloud run deploy") != nil {
			t.Error("deployed despite failed preflight")
		}
	})

	t.Run("not logged in", func(t *testing.T) {
		env, fc := newEnv(t, t.TempDir(), nil)
		fc.fail = []string{"gcloud auth"}
		res, _ := New().Apply(context.Background(), plan(t, s, env), s, env)
		if res.OK || res.Findings[0].Code != "GCP_PREFLIGHT_AUTH" {
			t.Errorf("result = %+v", res)
		}
	})

	t.Run("dry run", func(t *testing.T) {
		env, fc := newEnv(t, t.TempDir(), nil)
		env.DryRun = true
		res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
		if err != nil || !res.OK {
			t.Fatalf("dry run: %v %+v", err, res)
		}
		if fc.find("gcloud run deploy") != nil {
			t.Error("dry run deployed")
		}
	})
}

func TestStatus(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {
		"web": {"kind": "server", "image": "nginx"}, "api": {"kind": "server", "image": "api"}, "admin": {"kind": "server", "image": "admin"}}, `+target+`}`)
	env, fc := newEnv(t, t.TempDir(), nil)
	fc.out["gcloud run services list"] = `[
		{"metadata": {"name": "shop-web"}, "status": {"url": "https://web", "conditions": [{"type": "Ready", "status": "True"}]}},
		{"metadata": {"name": "shop-api"}, "status": {"conditions": [{"type": "Ready", "status": "False", "message": "container failed to start"}]}}]`
	st, err := New().Status(context.Background(), s, env)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, ss := range st.Services {
		got[ss.Name] = ss.State
	}
	want := map[string]string{"web": "running", "api": "failing", "admin": "missing"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if list := fc.find("gcloud run services list"); !slices.Contains(list.args, "metadata.labels.anyship-project=shop") {
		t.Errorf("list = %v", list.args)
	}
}

func TestLogs(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}}, `+target+`}`)
	env, fc := newEnv(t, t.TempDir(), nil)
	if err := New().Logs(context.Background(), s, env, adapter.LogOptions{Tail: 20, Since: "10m"}); err != nil {
		t.Fatal(err)
	}
	if fc.find("gcloud run services logs read shop-web --region us-central1 --limit 20 --freshness 10m") == nil {
		t.Errorf("calls = %v", fc.calls)
	}
	if err := New().Logs(context.Background(), s, env, adapter.LogOptions{Follow: true}); err == nil || !strings.Contains(err.Error(), "logs tail shop-web") {
		t.Errorf("follow err = %v", err)
	}
	if err := New().Logs(context.Background(), s, env, adapter.LogOptions{Since: "2026-01-01T00:00:00Z"}); err == nil {
		t.Error("timestamp --since should be refused")
	}
}

func TestDestroy(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {
		"web": {"kind": "server", "image": "nginx", "secrets": ["TOKEN"]}, "api": {"kind": "server", "image": "api"}},
		"secrets": {"TOKEN": {}}, `+target+`}`)
	env, fc := newEnv(t, t.TempDir(), nil)
	fc.out["gcloud run services list"] = `[{"metadata": {"name": "shop-web"}}]`
	res, err := New().Destroy(context.Background(), s, env, adapter.DestroyOptions{Volumes: true})
	if err != nil || !res.OK {
		t.Fatalf("destroy: %v %+v", err, res)
	}
	if fc.find("gcloud run services delete shop-web") == nil || fc.find("gcloud run services delete shop-api") != nil {
		t.Errorf("deletes wrong services: %v", fc.calls)
	}
	if fc.find("gcloud secrets delete shop-TOKEN") == nil {
		t.Error("secret not deleted with --volumes")
	}

	summary, err := New().DestroySummary(s, adapter.DestroyOptions{})
	if err != nil || !strings.Contains(strings.Join(summary, "\n"), "Keep the Secret Manager secrets shop-TOKEN") {
		t.Errorf("summary = %v %v", summary, err)
	}
}

func TestListFlag(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{[]string{"A=1", "B=2"}, "A=1,B=2"},
		{[]string{"A=1,2", "B=3"}, "^|^A=1,2|B=3"},
		{[]string{"A=1,|", "B=3"}, "^;^A=1,|;B=3"},
	} {
		if got := listFlag(tc.in); got != tc.want {
			t.Errorf("listFlag(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
