package kubernetes

import (
	"context"
	"encoding/base64"
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

// line is the command without kubectl's context and namespace flags, so
// tests match on what is done rather than where.
func (c call) line() string {
	args := c.args
	for len(args) >= 2 && (args[0] == "--context" || args[0] == "--namespace") {
		args = args[2:]
	}
	return c.name + " " + strings.Join(args, " ")
}

// fakeCluster answers commands by prefix, like a reachable cluster with a
// namespace this login may deploy to.
type fakeCluster struct {
	calls []call
	// out maps a command prefix to its stdout.
	out map[string]string
	// fail maps command prefixes that exit non-zero to what they say.
	fail map[string]string
	// digest is what the fake buildx reports.
	digest string
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{
		out: map[string]string{
			"kubectl version":                  "Client Version: v1.33.0",
			"kubectl config current-context":   "kind-anyship",
			"kubectl config get-contexts":      "orbstack",
			"kubectl get namespace":            "namespace/apps",
			"kubectl auth can-i":               "yes",
			"kubectl get nodes":                "amd64",
			"kubectl get deployments,services": "",
			"kubectl get deployments":          `{"items": []}`,
			"kubectl get pods":                 `{"items": []}`,
			"kubectl get secret":               "4711",
		},
		fail:   map[string]string{"kubectl get secret shop-api-key -o name": `Error from server (NotFound): secrets "shop-api-key" not found`},
		digest: "sha256:" + strings.Repeat("a", 64),
	}
}

func (f *fakeCluster) exec(_ context.Context, opts adapter.ExecOptions, name string, args ...string) error {
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
	for prefix, stderr := range f.fail {
		if strings.HasPrefix(line, prefix) {
			if opts.Stderr != nil {
				_, _ = io.WriteString(opts.Stderr, stderr+"\n")
			}
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
		// The longest prefix wins, so "kubectl get deployments,services"
		// isn't answered as "kubectl get deployments".
		best := ""
		for prefix := range f.out {
			if strings.HasPrefix(line, prefix) && len(prefix) > len(best) {
				best = prefix
			}
		}
		if best != "" {
			_, _ = io.WriteString(opts.Stdout, f.out[best])
		}
	}
	return nil
}

func (f *fakeCluster) find(prefix string) *call {
	for i := range f.calls {
		if strings.HasPrefix(f.calls[i].line(), prefix) {
			return &f.calls[i]
		}
	}
	return nil
}

func (f *fakeCluster) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c.line(), prefix) {
			n++
		}
	}
	return n
}

func newEnv(t *testing.T, dir string, env map[string]string) (*adapter.Env, *fakeCluster) {
	t.Helper()
	fc := newFakeCluster()
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

func apply(t *testing.T, s *spec.Spec, env *adapter.Env) *adapter.Result {
	t.Helper()
	p := plan(t, s, env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Fatalf("plan has errors: %v", errs)
	}
	r, err := New().Apply(context.Background(), p, s, env)
	if err != nil {
		t.Fatal(err)
	}
	return r
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

func resultCodes(r *adapter.Result) []string {
	var out []string
	for _, f := range r.Findings {
		out = append(out, f.Code)
	}
	return out
}

// manifestsOf is the file the plan would send to kubectl.
func manifestsOf(t *testing.T, p *adapter.Plan) string {
	t.Helper()
	for _, f := range p.Files {
		if filepath.Base(f.Path) == manifestsFile {
			return string(f.Contents)
		}
	}
	t.Fatal("the plan has no manifests file")
	return ""
}

const target = `"targets": {"kubernetes": {"context": "orbstack", "namespace": "apps", "repository": "localhost:5000/shop"}}`

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
		"services": {
			"web": {"kind": "server", "image": "nginx:1.27.0", "ports": [{"port": 80}], "replicas": 2, "memory": "512MB", "cpu": 0.5,
				"env": {"MODE": "prod", "API": "${services.api.url}/v1"}, "secrets": ["API_KEY"], "start": "nginx -g 'daemon off;'"},
			"api": {"kind": "server", "image": "ghcr.io/acme/api@sha256:`+strings.Repeat("b", 64)+`", "ports": [{"port": 9000, "exposure": "internal"}]}},
		"secrets": {"API_KEY": {}}, `+target+`}`)
	env, _ := newEnv(t, t.TempDir(), nil)
	p := plan(t, s, env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if warnings := codes(p, adapter.Warning); !slices.Equal(warnings, []string{"K8S_PUBLIC_PORT"}) {
		t.Errorf("warnings = %v, want only K8S_PUBLIC_PORT for the public port", warnings)
	}
	m := manifestsOf(t, p)
	for _, want := range []string{
		`"name": "shop-web"`, `"namespace": "apps"`, `"anyship-project": "shop"`, `"anyship-service": "web"`,
		`"replicas": 2`, `"image": "nginx:1.27.0"`, `"imagePullPolicy": "IfNotPresent"`,
		`"args": [`, `"nginx",`, `"-g",`, `"daemon off;"`,
		`"containerPort": 80`, `"name": "API",`, `"value": "http://shop-api.apps.svc:9000/v1"`, `"name": "PORT",`, `"value": "80"`,
		`"cpu": "0.5"`, `"memory": "512Mi"`, `"limits"`, `"requests"`,
		`"mountPath": "/run/secrets/API_KEY"`, `"subPath": "API_KEY"`, `"secretName": "shop-api-key"`, `"anyship.dev/secrets": "<resource versions of the secrets>"`,
		`"kind": "Service"`, `"type": "ClusterIP"`, `"targetPort": 80`,
		`"name": "shop-api"`, `"containerPort": 9000`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifests lack %s:\n%s", want, m)
		}
	}
	if strings.Contains(m, rolloutAnnotation) {
		t.Error("a pinned image shouldn't get the rollout annotation")
	}
	var names []string
	for _, a := range p.Actions {
		names = append(names, a.Kind+" "+a.Name)
	}
	for _, want := range []string{"Secret shop-api-key", "Deployment and Service shop-api", "Deployment and Service shop-web"} {
		if !slices.Contains(names, want) {
			t.Errorf("actions %v lack %q", names, want)
		}
	}
}

// A tag that can move is pulled on every rollout, and every apply rolls the
// pods so that it is.
func TestPlanMovingTag(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx", "ports": [{"port": 80, "exposure": "internal"}]}}, `+target+`}`)
	env, _ := newEnv(t, t.TempDir(), nil)
	p := plan(t, s, env)
	if !slices.Contains(codes(p, adapter.Info), "K8S_MUTABLE_TAG") {
		t.Errorf("infos = %v, want K8S_MUTABLE_TAG", codes(p, adapter.Info))
	}
	m := manifestsOf(t, p)
	for _, want := range []string{`"imagePullPolicy": "Always"`, `"anyship.dev/rollout": "<time of the apply>"`} {
		if !strings.Contains(m, want) {
			t.Errorf("manifests lack %s", want)
		}
	}
}

func TestPlanHealthCheck(t *testing.T) {
	for check, want := range map[string][]string{
		`{"path": "/healthz"}`:          {`"httpGet"`, `"path": "/healthz"`, `"port": 8080`, `"failureThreshold": 24`, `"failureThreshold": 3`, `"startupProbe"`, `"readinessProbe"`},
		`{"command": "redis-cli ping"}`: {`"exec"`, `"sh",`, `"-c",`, `"redis-cli ping"`},
	} {
		s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27.0", "healthCheck": `+check+`}}, `+target+`}`)
		env, _ := newEnv(t, t.TempDir(), nil)
		p := plan(t, s, env)
		if errs := codes(p, adapter.Error); len(errs) > 0 {
			t.Fatalf("%s: unexpected errors %v", check, errs)
		}
		if !slices.Contains(codes(p, adapter.Info), "K8S_HEALTH_CHECK") {
			t.Errorf("%s: infos = %v, want K8S_HEALTH_CHECK", check, codes(p, adapter.Info))
		}
		m := manifestsOf(t, p)
		for _, w := range want {
			if !strings.Contains(m, w) {
				t.Errorf("%s: manifests lack %s", check, w)
			}
		}
	}
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27.0", "healthCheck": {"path": "healthz"}}}, `+target+`}`)
	env, _ := newEnv(t, t.TempDir(), nil)
	if errs := codes(plan(t, s, env), adapter.Error); !slices.Equal(errs, []string{"K8S_BAD_HEALTH_PATH"}) {
		t.Errorf("errors = %v, want K8S_BAD_HEALTH_PATH", errs)
	}
	s = parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27.0"}}, `+target+`}`)
	if m := manifestsOf(t, plan(t, s, env)); strings.Contains(m, "Probe") {
		t.Error("a service without healthCheck shouldn't get probes")
	}
}

func TestPlanRefusesUnsupported(t *testing.T) {
	for src, want := range map[string]string{
		`"web": {"kind": "static", "path": "site"}`:                                                                        "K8S_STATIC",
		`"web": {"kind": "worker", "image": "busybox:1"}`:                                                                  "K8S_WORKER",
		`"web": {"kind": "server", "image": "nginx:1.27.0", "volumes": [{"name": "d", "mountPath": "/d", "size": "1GB"}]}`: "K8S_VOLUMES",
		`"web": {"kind": "server", "image": "nginx:1.27.0", "cron": [{"schedule": "* * * * *", "path": "/tick"}]}`:         "K8S_CRON",
		`"web": {"kind": "server", "image": "nginx:1.27.0", "ports": [{"port": 80}], "domains": ["shop.example.com"]}`:     "K8S_DOMAIN_UNSUPPORTED",
		`"web": {"kind": "server", "image": "nginx:1.27.0", "ports": [{"port": 53, "protocol": "udp"}]}`:                   "K8S_NON_HTTP_PORT",
		`"web": {"kind": "server", "image": "nginx:1.27.0", "ports": [{"port": 80}, {"port": 81}]}`:                        "K8S_MULTIPLE_PORTS",
		`"web": {"kind": "server", "image": "nginx:1.27.0", "uses": ["db"]}`:                                               "K8S_RESOURCE",
		`"web": {"kind": "server", "start": "node server.js"}`:                                                             "K8S_NEEDS_IMAGE",
		`"web": {"kind": "server", "image": "nginx:1.27.0", "start": "nginx 'unterminated"}`:                               "K8S_BAD_START",
	} {
		s := parse(t, `{"name": "shop", "services": {`+src+`}, "resources": {"db": {"type": "postgres"}}, `+target+`}`)
		env, _ := newEnv(t, t.TempDir(), nil)
		if errs := codes(plan(t, s, env), adapter.Error); !slices.Contains(errs, want) {
			t.Errorf("%s: errors = %v, want %s", src, errs, want)
		}
	}
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27.0", "uses": ["db"]}}, "resources": {"db": {"type": "postgres", "external": true}}, `+target+`}`)
	env, _ := newEnv(t, t.TempDir(), nil)
	p := plan(t, s, env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Errorf("an external resource shouldn't be refused: %v", errs)
	}
	if !slices.Contains(codes(p, adapter.Info), "K8S_EXTERNAL_RESOURCE") {
		t.Errorf("infos = %v, want K8S_EXTERNAL_RESOURCE", codes(p, adapter.Info))
	}
}

func TestPlanOptions(t *testing.T) {
	for options, want := range map[string]string{
		``:                      "namespace is required",
		`{"namespace": "Apps"}`: "not a valid namespace name",
		`{"namespace": "apps", "repository": "ghcr.io/me/app:v1"}`: "not a registry path",
		`{"namespace": "apps", "repository": "GHCR.io/me/app"}`:    "not a registry path",
		`{"namespace": "apps", "platform": "arm64"}`:               "not a platform",
		`{"namespace": "apps", "cluster": "x"}`:                    "unknown field",
	} {
		src := `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27.0"}}`
		if options != "" {
			src += `, "targets": {"kubernetes": ` + options + `}`
		}
		s := parse(t, src+`}`)
		env, _ := newEnv(t, t.TempDir(), nil)
		p := plan(t, s, env)
		if errs := codes(p, adapter.Error); !slices.Equal(errs, []string{"K8S_BAD_OPTIONS"}) {
			t.Errorf("%s: errors = %v, want K8S_BAD_OPTIONS", options, errs)
			continue
		}
		if !strings.Contains(p.Findings[0].Message, want) {
			t.Errorf("%s: message %q doesn't say %q", options, p.Findings[0].Message, want)
		}
	}
	for _, options := range []string{
		`{"namespace": "apps"}`,
		`{"namespace": "apps", "context": "gke_proj_asia-east1_main", "repository": "asia-east1-docker.pkg.dev/proj/apps/shop", "platform": "linux/arm64"}`,
		`{"namespace": "apps", "repository": "localhost:5000/shop"}`,
	} {
		s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27.0"}}, "targets": {"kubernetes": `+options+`}}`)
		env, _ := newEnv(t, t.TempDir(), nil)
		if errs := codes(plan(t, s, env), adapter.Error); len(errs) > 0 {
			t.Errorf("%s: unexpected errors %v", options, errs)
		}
	}
}

func TestPlanBuildNeedsRepository(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "start": "node server.js"}}, "targets": {"kubernetes": {"namespace": "apps"}}}`)
	env, _ := newEnv(t, nodeApp(t), nil)
	if errs := codes(plan(t, s, env), adapter.Error); !slices.Equal(errs, []string{"K8S_NO_REPOSITORY"}) {
		t.Errorf("errors = %v, want K8S_NO_REPOSITORY", errs)
	}
}

func TestPlanGeneratesDockerfile(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "start": "node server.js", "ports": [{"port": 3000, "exposure": "internal"}]}}, `+target+`}`)
	env, _ := newEnv(t, nodeApp(t), nil)
	p := plan(t, s, env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if !slices.Contains(codes(p, adapter.Info), "K8S_GENERATED_DOCKERFILE") {
		t.Errorf("infos = %v, want K8S_GENERATED_DOCKERFILE", codes(p, adapter.Info))
	}
	var files []string
	for _, f := range p.Files {
		files = append(files, filepath.Base(f.Path))
	}
	if !slices.Contains(files, "web.Dockerfile") {
		t.Errorf("files = %v, want web.Dockerfile", files)
	}
	if m := manifestsOf(t, p); !strings.Contains(m, `"image": "localhost:5000/shop/shop-web@<digest from the build>"`) {
		t.Errorf("manifests should show the image placeholder:\n%s", m)
	}
}

func TestApplyBuildsAndDeploysByDigest(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "start": "node server.js", "ports": [{"port": 3000, "exposure": "internal"}], "secrets": ["API_KEY", "TOKEN"]}},
		"secrets": {"API_KEY": {}, "TOKEN": {"generate": "hex32"}}, `+target+`}`)
	dir := nodeApp(t)
	env, fc := newEnv(t, dir, map[string]string{"API_KEY": "s3cret"})
	fc.fail["kubectl get secret shop-token -o name"] = `Error from server (NotFound): secrets "shop-token" not found`
	r := apply(t, s, env)
	if !r.OK {
		t.Fatalf("apply failed: %v", r.Messages)
	}

	// The secret from the environment is written, the generated one made up.
	secrets := 0
	for _, c := range fc.calls {
		if !strings.HasPrefix(c.line(), "kubectl apply -f -") || strings.Contains(c.line(), "--prune") {
			continue
		}
		secrets++
		switch {
		case strings.Contains(c.stdin, `"shop-api-key"`):
			if !strings.Contains(c.stdin, base64.StdEncoding.EncodeToString([]byte("s3cret"))) {
				t.Errorf("API_KEY secret lacks the value from the environment: %s", c.stdin)
			}
		case strings.Contains(c.stdin, `"shop-token"`):
			if !strings.Contains(c.stdin, `"TOKEN": "`) || strings.Contains(c.stdin, `"TOKEN": ""`) {
				t.Errorf("TOKEN secret should carry a generated value: %s", c.stdin)
			}
		default:
			t.Errorf("unexpected secret apply: %s", c.stdin)
		}
		if !strings.Contains(c.stdin, `"anyship-project": "shop"`) {
			t.Errorf("secret lacks the project label: %s", c.stdin)
		}
	}
	if secrets != 2 {
		t.Errorf("%d secrets written, want 2", secrets)
	}

	build := fc.find("docker buildx build")
	if build == nil {
		t.Fatal("no docker buildx build call")
	}
	if line := build.line(); !strings.Contains(line, "--platform linux/amd64") || !strings.Contains(line, "--tag localhost:5000/shop/shop-web:latest") || !strings.Contains(line, "--push") {
		t.Errorf("buildx args: %s", line)
	}

	a := fc.find("kubectl apply -f - --prune")
	if a == nil {
		t.Fatal("no kubectl apply --prune call")
	}
	if line := a.line(); !strings.Contains(line, "-l anyship-project=shop") || !strings.Contains(line, "--prune-allowlist apps/v1/Deployment") || !strings.Contains(line, "--prune-allowlist core/v1/Service") {
		t.Errorf("apply args: %s", line)
	}
	if !slices.Equal(a.args[:4], []string{"--context", "orbstack", "--namespace", "apps"}) {
		t.Errorf("apply should name the context and namespace: %v", a.args)
	}
	if !strings.Contains(a.stdin, `"image": "localhost:5000/shop/shop-web@`+fc.digest+`"`) {
		t.Errorf("the manifests sent should deploy the pushed digest:\n%s", a.stdin)
	}
	if !strings.Contains(a.stdin, `"anyship.dev/secrets": "API_KEY=4711,TOKEN=4711"`) {
		t.Errorf("the pod template should carry the secrets' resource versions:\n%s", a.stdin)
	}
	written, err := os.ReadFile(filepath.Join(env.OutDir, manifestsFile))
	if err != nil || !strings.Contains(string(written), fc.digest) {
		t.Errorf("the manifests file should be rewritten with the digest (%v)", err)
	}
	if fc.find("kubectl rollout status deployment/shop-web --timeout 5m0s") == nil {
		t.Error("apply should wait for the rollout")
	}
	if !slices.Contains(r.Messages, "web: http://shop-web.apps.svc:3000 (inside the cluster)") {
		t.Errorf("messages = %v", r.Messages)
	}
}

func TestApplyKeepsAnExistingSecret(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27.0", "secrets": ["API_KEY"]}}, "secrets": {"API_KEY": {}}, `+target+`}`)
	env, fc := newEnv(t, t.TempDir(), nil)
	delete(fc.fail, "kubectl get secret shop-api-key -o name")
	fc.out["kubectl get secret shop-api-key -o name"] = "secret/shop-api-key"
	if r := apply(t, s, env); !r.OK {
		t.Fatalf("apply failed: %v", r.Messages)
	}
	if fc.count("kubectl apply -f -") != 1 {
		t.Error("an existing secret without a value in the environment should be left alone")
	}
}

func TestApplyMissingSecretFails(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27.0", "secrets": ["API_KEY"]}}, "secrets": {"API_KEY": {}}, `+target+`}`)
	env, fc := newEnv(t, t.TempDir(), nil)
	r := apply(t, s, env)
	if r.OK || !strings.Contains(r.Messages[0], "export API_KEY") {
		t.Errorf("result = %+v, want a failure asking for $API_KEY", r)
	}
	if fc.find("kubectl apply -f - --prune") != nil {
		t.Error("nothing should be applied without the secret")
	}
}

func TestApplyPlatformFollowsTheNodes(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "start": "node server.js"}}, `+target+`}`)
	for nodes, want := range map[string]string{"arm64": "linux/arm64", "arm64 arm64": "linux/arm64", "amd64 arm64": "linux/amd64", "": "linux/amd64"} {
		env, fc := newEnv(t, nodeApp(t), nil)
		fc.out["kubectl get nodes"] = nodes
		r := apply(t, s, env)
		if !r.OK {
			t.Fatalf("%q: apply failed: %v", nodes, r.Messages)
		}
		if line := fc.find("docker buildx build").line(); !strings.Contains(line, "--platform "+want) {
			t.Errorf("nodes %q: built for %s, want %s", nodes, line, want)
		}
		if noted := nodes == "" || nodes == "amd64 arm64"; noted != slices.Contains(resultCodes(r), "K8S_PREFLIGHT_PLATFORM") {
			t.Errorf("nodes %q: findings = %v", nodes, resultCodes(r))
		}
	}
	s = parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "start": "node server.js"}}, "targets": {"kubernetes": {"namespace": "apps", "repository": "localhost:5000/shop", "platform": "linux/arm64"}}}`)
	env, fc := newEnv(t, nodeApp(t), nil)
	if r := apply(t, s, env); !r.OK {
		t.Fatalf("apply failed: %v", r.Messages)
	}
	if line := fc.find("docker buildx build").line(); !strings.Contains(line, "--platform linux/arm64") {
		t.Errorf("the platform option should win: %s", line)
	}
	if fc.find("kubectl get nodes") != nil {
		t.Error("the nodes needn't be read when the platform is set")
	}
}

func TestApplyPreflight(t *testing.T) {
	src := `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27.0"}}, `
	for name, tc := range map[string]struct {
		options string
		fail    map[string]string
		out     map[string]string
		want    string
	}{
		"no kubectl":         {fail: map[string]string{"kubectl version": "command not found"}, want: "K8S_PREFLIGHT_KUBECTL"},
		"unknown context":    {fail: map[string]string{"kubectl config get-contexts": `error: context "orbstack" not found`}, want: "K8S_PREFLIGHT_CONTEXT"},
		"no current context": {options: `{"namespace": "apps"}`, fail: map[string]string{"kubectl config current-context": "error: current-context is not set"}, want: "K8S_PREFLIGHT_CONTEXT"},
		"missing namespace":  {fail: map[string]string{"kubectl get namespace": `Error from server (NotFound): namespaces "apps" not found`}, want: "K8S_PREFLIGHT_NAMESPACE"},
		"unreachable":        {fail: map[string]string{"kubectl get namespace": "The connection to the server 127.0.0.1:6443 was refused"}, want: "K8S_PREFLIGHT_CLUSTER"},
		"no access":          {out: map[string]string{"kubectl auth can-i": "no"}, want: "K8S_PREFLIGHT_ACCESS"},
	} {
		full := src + target + "}"
		if tc.options != "" {
			full = src + `"targets": {"kubernetes": ` + tc.options + `}}`
		}
		s := parse(t, full)
		env, fc := newEnv(t, t.TempDir(), nil)
		for prefix, stderr := range tc.fail {
			fc.fail[prefix] = stderr
		}
		for prefix, out := range tc.out {
			fc.out[prefix] = out
		}
		r := apply(t, s, env)
		if r.OK || !slices.Contains(resultCodes(r), tc.want) {
			t.Errorf("%s: ok=%v findings=%v, want %s", name, r.OK, resultCodes(r), tc.want)
		}
		if fc.find("kubectl apply") != nil {
			t.Errorf("%s: nothing should be applied after a failed check", name)
		}
	}

	s := parse(t, src+target+"}")
	env, fc := newEnv(t, t.TempDir(), nil)
	fc.fail["kubectl get namespace"] = `Error from server (NotFound): namespaces "apps" not found`
	r := apply(t, s, env)
	var hint string
	for _, f := range r.Findings {
		if f.Code == "K8S_PREFLIGHT_NAMESPACE" {
			hint = f.Hint
		}
	}
	if !strings.Contains(hint, "kubectl --context orbstack create namespace apps") {
		t.Errorf("hint %q should say how to create the namespace", hint)
	}

	env, fc = newEnv(t, t.TempDir(), nil)
	env.DryRun = true
	r = apply(t, s, env)
	if !r.OK || !slices.Contains(resultCodes(r), "K8S_PREFLIGHT_OK") || fc.find("kubectl apply") != nil {
		t.Errorf("dry run: ok=%v findings=%v calls=%d", r.OK, resultCodes(r), len(fc.calls))
	}
}

func TestApplyReportsAFailedRollout(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27.0"}}, `+target+`}`)
	env, fc := newEnv(t, t.TempDir(), nil)
	fc.fail["kubectl rollout status"] = "error: timed out waiting for the condition"
	fc.out["kubectl get pods"] = "shop-web-6d4b-abcde   0/1   ImagePullBackOff   0   2m"
	fc.out["kubectl get events"] = "2m   Warning   Failed   pod/shop-web-6d4b-abcde   Failed to pull image\n1m   Warning   Other   pod/other-pod   unrelated"
	r := apply(t, s, env)
	if r.OK {
		t.Fatal("a failed rollout should fail the apply")
	}
	joined := strings.Join(r.Messages, "\n")
	for _, want := range []string{"The rollout of shop-web didn't finish", "ImagePullBackOff", "Failed to pull image", "anyship logs -t kubernetes web"} {
		if !strings.Contains(joined, want) {
			t.Errorf("messages lack %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "unrelated") {
		t.Error("warnings about other objects shouldn't be shown")
	}
}

func TestStatus(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {
		"web": {"kind": "server", "image": "nginx:1.27.0", "ports": [{"port": 80}], "replicas": 2, "healthCheck": {"path": "/"}},
		"api": {"kind": "server", "image": "nginx:1.27.0"},
		"old": {"kind": "server", "image": "nginx:1.27.0"}}, `+target+`}`)
	env, fc := newEnv(t, t.TempDir(), nil)
	fc.out["kubectl get deployments"] = `{"items": [
		{"metadata": {"name": "shop-web"}, "spec": {"replicas": 2}, "status": {"availableReplicas": 2, "updatedReplicas": 2, "conditions": [{"type": "Available", "status": "True"}]}},
		{"metadata": {"name": "shop-api"}, "spec": {"replicas": 1}, "status": {"availableReplicas": 0, "updatedReplicas": 1, "conditions": [{"type": "Progressing", "status": "True"}]}}]}`
	fc.out["kubectl get pods"] = `{"items": [
		{"metadata": {"labels": {"anyship-service": "api"}}, "status": {"containerStatuses": [{"ready": false, "state": {"waiting": {"reason": "CrashLoopBackOff", "message": "back-off 5m restarting failed container"}}}]}}]}`
	st, err := New().Status(context.Background(), s, env)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Deployed || st.Location != "orbstack/apps" || st.Healthy() {
		t.Errorf("status = %+v", st)
	}
	byName := map[string]adapter.ServiceStatus{}
	for _, ss := range st.Services {
		byName[ss.Name] = ss
	}
	if web := byName["web"]; web.State != "running" || web.Running != 2 || web.Desired != 2 || web.Health != "healthy" || !slices.Equal(web.Ports, []string{"80/http"}) {
		t.Errorf("web = %+v", web)
	}
	if api := byName["api"]; api.State != "CrashLoopBackOff" || api.Health != "unhealthy" || !strings.Contains(api.Detail, "back-off") {
		t.Errorf("api = %+v", api)
	}
	if old := byName["old"]; old.State != "missing" || old.Desired != 1 {
		t.Errorf("old = %+v", old)
	}
}

func TestLogs(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27.0"}}, `+target+`}`)
	env, fc := newEnv(t, t.TempDir(), nil)
	err := New().Logs(context.Background(), s, env, adapter.LogOptions{Service: "web", Follow: true, Tail: 20, Since: "10m", Timestamps: true})
	if err != nil {
		t.Fatal(err)
	}
	line := fc.calls[0].line()
	for _, want := range []string{"kubectl logs -l anyship-project=shop,anyship-service=web", "--all-containers", "--prefix", "--tail 20", "--since 10m", "--timestamps", "--follow"} {
		if !strings.Contains(line, want) {
			t.Errorf("logs args %q lack %q", line, want)
		}
	}
	fc.calls = nil
	if err := New().Logs(context.Background(), s, env, adapter.LogOptions{Since: "2026-10-07T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if line := fc.calls[0].line(); !strings.Contains(line, "--since-time 2026-10-07T00:00:00Z") || !strings.Contains(line, "--tail 100") || strings.Contains(line, "anyship-service") {
		t.Errorf("logs args: %s", line)
	}
}

func TestDestroy(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27.0", "secrets": ["API_KEY"]}}, "secrets": {"API_KEY": {}}, `+target+`}`)
	a := New()
	summary, err := a.DestroySummary(s, adapter.DestroyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(summary, "\n")
	for _, want := range []string{"Delete the Deployments and Services shop-web in namespace apps of orbstack.", "Keep the Secrets shop-api-key"} {
		if !strings.Contains(joined, want) {
			t.Errorf("summary lacks %q:\n%s", want, joined)
		}
	}
	summary, _ = a.DestroySummary(s, adapter.DestroyOptions{Volumes: true})
	if !strings.Contains(strings.Join(summary, "\n"), "DELETE the Secrets shop-api-key") {
		t.Errorf("summary with --volumes: %v", summary)
	}

	env, fc := newEnv(t, t.TempDir(), nil)
	r, err := a.Destroy(context.Background(), s, env, adapter.DestroyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK || !strings.Contains(r.Messages[0], "Nothing to remove") || fc.find("kubectl delete") != nil {
		t.Errorf("destroy of nothing: %+v, calls %d", r, len(fc.calls))
	}

	env, fc = newEnv(t, t.TempDir(), nil)
	fc.out["kubectl get deployments,services"] = "deployment.apps/shop-web\nservice/shop-web"
	r, err = a.Destroy(context.Background(), s, env, adapter.DestroyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK || fc.find("kubectl delete deployments,services -l anyship-project=shop --ignore-not-found") == nil {
		t.Errorf("destroy: %+v", r)
	}
	env, fc = newEnv(t, t.TempDir(), nil)
	if _, err := a.Destroy(context.Background(), s, env, adapter.DestroyOptions{Volumes: true}); err != nil {
		t.Fatal(err)
	}
	if fc.find("kubectl delete deployments,services,secrets -l anyship-project=shop") == nil {
		t.Error("destroy --volumes should delete the secrets too")
	}
}

func TestDeployOrderFollowsDependsOn(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {
		"web": {"kind": "server", "image": "nginx:1.27.0", "dependsOn": ["api"]},
		"api": {"kind": "server", "image": "nginx:1.27.0"}}, `+target+`}`)
	env, fc := newEnv(t, t.TempDir(), nil)
	p := plan(t, s, env)
	if !slices.Contains(codes(p, adapter.Info), "K8S_DEPENDS_ON") {
		t.Errorf("infos = %v, want K8S_DEPENDS_ON", codes(p, adapter.Info))
	}
	if r := apply(t, s, env); !r.OK {
		t.Fatal(r.Messages)
	}
	var waits []string
	for _, c := range fc.calls {
		if strings.HasPrefix(c.line(), "kubectl rollout status") {
			waits = append(waits, c.args[len(c.args)-3])
		}
	}
	if !slices.Equal(waits, []string{"deployment/shop-api", "deployment/shop-web"}) {
		t.Errorf("rollouts waited for in order %v", waits)
	}
}
