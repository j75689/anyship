package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/internal/exectest"
	"github.com/j75689/anyship/internal/spectest"
	"github.com/j75689/anyship/spec"
)

const dir = "/work/app"

func newEnv() *adapter.Env {
	return &adapter.Env{
		Dir:    dir,
		OutDir: filepath.Join(dir, ".anyship", "cloudflare"),
		Logf:   func(string, ...any) {},
		Exec: func(context.Context, adapter.ExecOptions, string, ...string) error {
			return errors.New("unexpected exec")
		},
	}
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

func codes(p *adapter.Plan) []string {
	var out []string
	for _, f := range p.Findings {
		out = append(out, f.Code)
	}
	return out
}

// rendered decodes the generated wrangler.jsonc, skipping its comment line.
func rendered(t *testing.T, p *adapter.Plan) map[string]any {
	t.Helper()
	if len(p.Files) != 1 {
		t.Fatalf("want 1 generated file, got %d (findings %v)", len(p.Files), codes(p))
	}
	_, body, _ := bytes.Cut(p.Files[0].Contents, []byte("\n"))
	var config map[string]any
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func jsonEqual(t *testing.T, got any, want string) {
	t.Helper()
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, w) {
		g, _ := json.Marshal(got)
		t.Errorf("got  %s\nwant %s", g, want)
	}
}

func TestRendersWorkerForEdgeServer(t *testing.T) {
	p := plan(t, parse(t, `{
		"name": "api",
		"services": {"web": {"kind": "server", "entry": "src/index.ts", "env": {"MODE": "prod"}, "cron": [{"schedule": "0 * * * *"}]}},
		"targets": {"cloudflare": {"domains": ["api.example.com"]}}
	}`), newEnv())

	if len(p.Findings) != 0 {
		t.Errorf("findings = %v", codes(p))
	}
	// A generated file's path is a local path, so it uses the local separator.
	if want := filepath.Join(dir, ".anyship", "cloudflare", "wrangler.jsonc"); p.Files[0].Path != want {
		t.Errorf("path = %s, want %s", p.Files[0].Path, want)
	}
	jsonEqual(t, rendered(t, p), `{
		"name": "api",
		"compatibility_date": "`+DefaultCompatibilityDate+`",
		"compatibility_flags": ["nodejs_compat"],
		"main": "../../src/index.ts",
		"vars": {"MODE": "prod"},
		"triggers": {"crons": ["0 * * * *"]},
		"routes": [{"pattern": "api.example.com", "custom_domain": true}]
	}`)
	if last := p.Actions[len(p.Actions)-1]; last.Op != adapter.OpDeploy || last.Name != "api" {
		t.Errorf("last action = %+v", last)
	}
}

func TestRendersStaticAssetsAndBuildsFirst(t *testing.T) {
	p := plan(t, parse(t, `{
		"name": "site",
		"services": {"web": {"kind": "static", "path": "apps/site", "build": {"command": "npm run build", "output": "dist"}}},
		"targets": {"cloudflare": {"spa": true}}
	}`), newEnv())

	jsonEqual(t, rendered(t, p)["assets"], `{"directory": "../../apps/site/dist", "not_found_handling": "single-page-application"}`)
	var ops []adapter.Op
	for _, a := range p.Actions {
		ops = append(ops, a.Op)
	}
	if !slices.Equal(ops, []adapter.Op{adapter.OpRun, adapter.OpDeploy}) {
		t.Errorf("ops = %v", ops)
	}
}

func TestBindsResourcesAndAsksForMissingIDs(t *testing.T) {
	const base = `"name": "api",
		"services": {"web": {"kind": "server", "entry": "src/index.ts", "uses": ["db", "files", "cache"]}},
		"resources": {"db": {"type": "sqlite"}, "files": {"type": "bucket"}, "cache": {"type": "kv"}}`

	missing := plan(t, parse(t, `{`+base+`}`), newEnv())
	if got := codes(missing); !slices.Equal(got, []string{"CF_MISSING_RESOURCE_ID", "CF_R2_BUCKET", "CF_MISSING_RESOURCE_ID"}) {
		t.Errorf("findings = %v", got)
	}
	if len(missing.Files) != 0 {
		t.Error("a plan with errors must not generate files")
	}
	if !slices.ContainsFunc(missing.Actions, func(a adapter.Action) bool {
		return a.Op == adapter.OpCreate && a.Detail == "npx wrangler d1 create api-db"
	}) {
		t.Errorf("actions = %+v", missing.Actions)
	}

	bound := rendered(t, plan(t, parse(t, `{`+base+`,
		"targets": {"cloudflare": {"bindings": {"db": {"id": "d1-123"}, "cache": {"id": "kv-456"}}}}}`), newEnv()))
	jsonEqual(t, bound["d1_databases"], `[{"binding": "DB", "database_name": "api-db", "database_id": "d1-123"}]`)
	jsonEqual(t, bound["kv_namespaces"], `[{"binding": "CACHE", "id": "kv-456"}]`)
	jsonEqual(t, bound["r2_buckets"], `[{"binding": "FILES", "bucket_name": "api-files"}]`)
}

func TestAttachesServiceDomainsAndHonorsTheOverride(t *testing.T) {
	const service = `"services": {"web": {"kind": "server", "entry": "src/index.ts",
		"ports": [{"port": 8080}], "domains": ["api.example.com", "www.example.com"]}}`

	fromSpec := plan(t, parse(t, `{"name": "api", `+service+`}`), newEnv())
	if len(fromSpec.Findings) != 0 {
		t.Errorf("findings = %v", codes(fromSpec))
	}
	jsonEqual(t, rendered(t, fromSpec)["routes"], `[
		{"pattern": "api.example.com", "custom_domain": true},
		{"pattern": "www.example.com", "custom_domain": true}]`)

	// targets.cloudflare.domains replaces the service's own domains, with a note.
	overridden := plan(t, parse(t, `{"name": "api", `+service+`,
		"targets": {"cloudflare": {"domains": ["staging.example.com"]}}}`), newEnv())
	jsonEqual(t, rendered(t, overridden)["routes"], `[{"pattern": "staging.example.com", "custom_domain": true}]`)
	if got := codes(overridden); !slices.Equal(got, []string{"CF_DOMAIN_OVERRIDE"}) {
		t.Errorf("codes = %v", got)
	}
}

// A Worker's cron trigger calls scheduled(); a path or a command in the
// entry can't be honoured, and plan says so.
func TestRefusesServiceURLRefs(t *testing.T) {
	p := plan(t, parse(t, `{"name": "app",
		"services": {"web": {"kind": "server", "entry": "src/index.ts", "env": {"SELF": "${services.web.url}"}}},
		"targets": {"cloudflare": {}}}`), newEnv())
	if got := codes(p); !slices.Contains(got, "CF_SERVICE_URL") {
		t.Errorf("codes = %v, want CF_SERVICE_URL", got)
	}
}

func TestWarnsAboutCronPathsAndCommands(t *testing.T) {
	p := plan(t, parse(t, `{"name": "app",
		"services": {"web": {"kind": "server", "entry": "src/index.ts", "cron": [
			{"schedule": "* * * * *", "path": "/tick"}, {"schedule": "0 * * * *", "command": "./job"}, {"schedule": "0 0 * * *"}]}},
		"targets": {"cloudflare": {}}}`), newEnv())
	got := codes(p)
	for _, want := range []string{"CF_CRON_PATH", "CF_CRON_COMMAND"} {
		if !slices.Contains(got, want) {
			t.Errorf("codes = %v, want %s", got, want)
		}
	}
}

func TestRefusesWhatWorkersCannotRun(t *testing.T) {
	eth, err := spec.Load(filepath.Join("..", "..", "examples", "ethereum-node", spec.Filename))
	if err != nil {
		t.Fatal(err)
	}
	p := plan(t, eth, newEnv())
	got := codes(p)
	slices.Sort(got)
	got = slices.Compact(got)
	if want := []string{"CF_CONTAINER_IMAGE", "CF_MULTI_SERVICE", "CF_NON_HTTP_PORT", "CF_VOLUMES"}; !slices.Equal(got, want) {
		t.Errorf("codes = %v, want %v", got, want)
	}
	if len(p.Files) != 0 {
		t.Error("a refused plan must not generate files")
	}
}

func TestRefusesNodeOnlyCodeAndBadOptions(t *testing.T) {
	nodeOnly := plan(t, parse(t, `{"name": "api",
		"services": {"web": {"kind": "server", "start": "node server.js", "runtime": {"edgeCompatible": false}}}}`), newEnv())
	if got := codes(nodeOnly); !slices.Equal(got, []string{"CF_EDGE_INCOMPATIBLE"}) {
		t.Errorf("codes = %v", got)
	}

	typo := plan(t, parse(t, `{"name": "api",
		"services": {"web": {"kind": "server", "entry": "a.ts"}},
		"targets": {"cloudflare": {"domain": ["x.com"]}}}`), newEnv())
	if got := codes(typo); !slices.Equal(got, []string{"CF_BAD_OPTIONS"}) || !strings.Contains(typo.Findings[0].Message, `"domain"`) {
		t.Errorf("findings = %+v", typo.Findings)
	}
}

func TestApplyWritesConfigBuildsThenDeploys(t *testing.T) {
	env := newEnv()
	env.Dir = t.TempDir()
	env.OutDir = filepath.Join(env.Dir, ".anyship", "cloudflare")
	env.DryRun = true
	var calls []string
	env.Exec = func(_ context.Context, opts adapter.ExecOptions, name string, args ...string) error {
		line := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, line)
		if line == "npx --no-install wrangler whoami --json" {
			_, _ = io.WriteString(opts.Stdout, `{"loggedIn": true}`)
		}
		return nil
	}

	s := parse(t, `{"name": "site", "services": {"web": {"kind": "static", "build": {"command": "make", "output": "out"}}}}`)
	result, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(env.OutDir, "wrangler.jsonc")
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	if want := []string{"node --version", "npx --no-install wrangler --version", "npx --no-install wrangler whoami --json", "make", "npx wrangler deploy --config " + configPath + " --dry-run"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	written, err := os.ReadFile(configPath)
	if err != nil || !bytes.Contains(written, []byte(`"name": "site"`)) {
		t.Errorf("config not written: %v %s", err, written)
	}
}

func TestApplyPreflight(t *testing.T) {
	s := parse(t, `{"name": "site", "services": {"web": {"kind": "static", "build": {"command": "make", "output": "out"}}}}`)
	missing := func(name string) error { return &exec.Error{Name: name, Err: exec.ErrNotFound} }
	loggedOut := func(line string) (string, error) {
		if line == "npx --no-install wrangler whoami --json" {
			return `{"loggedIn": false}`, errors.New("exit status 1")
		}
		return "4.1.0", nil
	}
	for _, tc := range []struct {
		name   string
		dryRun bool
		// answer returns the stdout, stderr and error of one command line.
		answer func(line string) (string, error)
		code   string
		level  adapter.Level
		// deploys says whether the build and the deploy run.
		deploys bool
	}{
		{"no node", false, func(line string) (string, error) {
			return "", missing(strings.Fields(line)[0])
		}, "CF_PREFLIGHT_NODE", adapter.Error, false},
		{"wrangler broken", false, func(line string) (string, error) {
			if strings.HasPrefix(line, "npx") {
				return "", errors.New("exit status 1")
			}
			return "v22.1.0", nil
		}, "CF_PREFLIGHT_WRANGLER", adapter.Error, false},
		// Not downloaded yet: the deploy's npx downloads it, asking first.
		{"wrangler not downloaded", false, func(line string) (string, error) {
			if strings.HasPrefix(line, "npx") {
				return "", errors.New("exit status 1: npx canceled due to missing packages and no YES option: [\"wrangler@4.149.0\"]")
			}
			return "v22.1.0", nil
		}, "CF_PREFLIGHT_WRANGLER", adapter.Info, true},
		{"logged out", false, loggedOut, "CF_PREFLIGHT_AUTH", adapter.Error, false},
		// wrangler deploy --dry-run needs no login.
		{"logged out, dry run", true, loggedOut, "CF_PREFLIGHT_AUTH", adapter.Warning, true},
		{"logged out, wrangler 3", false, func(line string) (string, error) {
			switch line {
			case "npx --no-install wrangler whoami --json":
				return "", errors.New("exit status 1")
			case "npx --no-install wrangler whoami":
				return "Getting User settings...\nYou are not authenticated. Please run `wrangler login`.", nil
			}
			return "3.114.0", nil
		}, "CF_PREFLIGHT_AUTH", adapter.Error, false},
		// An old wrangler is only a warning.
		{"old wrangler", false, func(line string) (string, error) {
			if line == "npx --no-install wrangler whoami --json" {
				return `{"loggedIn": true}`, nil
			}
			return "3.80.0", nil
		}, "CF_PREFLIGHT_VERSION", adapter.Warning, true},
		{"ready", false, func(line string) (string, error) {
			if line == "npx --no-install wrangler whoami --json" {
				return `{"loggedIn": true, "email": "dev@example.com"}`, nil
			}
			return " ⛅️ wrangler 4.1.0", nil
		}, "CF_PREFLIGHT_OK", adapter.Info, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv()
			env.Dir = t.TempDir()
			env.OutDir = filepath.Join(env.Dir, ".anyship", "cloudflare")
			env.DryRun = tc.dryRun
			var built bool
			env.Exec = func(_ context.Context, opts adapter.ExecOptions, name string, args ...string) error {
				line := strings.Join(append([]string{name}, args...), " ")
				if opts.Shell || strings.Contains(line, "deploy") {
					built = true
					return nil
				}
				if opts.Stdin == nil {
					t.Errorf("%s may wait on the terminal", line)
				}
				if strings.HasPrefix(line, "npx") && !strings.HasPrefix(line, "npx --no-install") {
					t.Errorf("%s may download wrangler", line)
				}
				out, err := tc.answer(line)
				_, _ = io.WriteString(opts.Stdout, out)
				if err != nil && strings.Contains(err.Error(), ": ") {
					msg := err.Error()[strings.Index(err.Error(), ": ")+2:]
					_, _ = io.WriteString(opts.Stderr, "npm error "+msg+"\n")
					err = errors.New("exit status 1")
				}
				return err
			}
			res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
			if err != nil || len(res.Findings) == 0 || res.Findings[0].Code != tc.code || res.Findings[0].Level != tc.level {
				t.Fatalf("result = %+v, %v", res, err)
			}
			if res.OK != tc.deploys || built != tc.deploys {
				t.Errorf("ok = %v, built = %v", res.OK, built)
			}
			if tc.code == "CF_PREFLIGHT_OK" && !strings.Contains(res.Findings[0].Message, "4.1.0") {
				t.Errorf("message = %q", res.Findings[0].Message)
			}
		})
	}
}

func TestApplyRefusesPlanWithErrors(t *testing.T) {
	s := parse(t, `{"name": "api", "services": {"web": {"kind": "worker", "start": "node job.js"}}}`)
	env := newEnv()
	result, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK {
		t.Error("apply should refuse a plan with errors")
	}
}

func TestLogsAndDestroyAddressTheWorkerFromTheSpecOnly(t *testing.T) {
	s := parse(t, `{"name": "api", "services": {"web": {"kind": "server", "entry": "src/index.ts"}},
		"targets": {"cloudflare": {"name": "api-prod", "accountId": "acc-123"}}}`)
	env := newEnv()
	env.OutDir = t.TempDir()
	var got []string
	var gotEnv []string
	env.Exec = func(_ context.Context, opts adapter.ExecOptions, name string, args ...string) error {
		got, gotEnv = append([]string{name}, args...), opts.Env
		return nil
	}
	// A config left by an earlier apply, under an old Worker name. anyship keeps
	// no state, so it must never be read back.
	stale := `{"name": "api-old", "account_id": "acc-old"}`
	if err := os.WriteFile(filepath.Join(env.OutDir, "wrangler.jsonc"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := New().Logs(context.Background(), s, env, adapter.LogOptions{Follow: true}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"npx", "wrangler", "tail", "api-prod", "--format", "pretty"}; !slices.Equal(got, want) {
		t.Errorf("logs ran %q, want %q", got, want)
	}
	if !slices.Equal(gotEnv, []string{"CLOUDFLARE_ACCOUNT_ID=acc-123"}) {
		t.Errorf("logs env = %q", gotEnv)
	}

	if _, err := New().Destroy(context.Background(), s, env, adapter.DestroyOptions{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"npx", "wrangler", "delete", "--name", "api-prod"}; !slices.Equal(got, want) {
		t.Errorf("destroy ran %q, want %q", got, want)
	}
	if !slices.Equal(gotEnv, []string{"CLOUDFLARE_ACCOUNT_ID=acc-123"}) {
		t.Errorf("destroy env = %q", gotEnv)
	}
}

func TestLogsRefusesHistoryOptions(t *testing.T) {
	s := parse(t, `{"name": "api", "services": {"web": {"kind": "server", "entry": "a.ts"}}}`)
	for _, opts := range []adapter.LogOptions{{Tail: 50}, {Since: "10m"}} {
		err := New().Logs(context.Background(), s, newEnv(), opts)
		if err == nil || !strings.Contains(err.Error(), "only streams live logs") {
			t.Errorf("opts %+v: err = %v", opts, err)
		}
	}
}

func TestRefusesNonJavaScriptServices(t *testing.T) {
	p := plan(t, parse(t, `{"name": "api",
		"services": {"web": {"kind": "server", "build": {"command": "go build -o bin/api ."}, "start": "./bin/api", "runtime": {"language": "go"}}}}`), newEnv())
	if got := codes(p); !slices.Equal(got, []string{"CF_LANGUAGE"}) {
		t.Errorf("codes = %v", got)
	}
	if !strings.Contains(p.Findings[0].Message, "go service") || !strings.Contains(p.Findings[0].Hint, "vps") {
		t.Errorf("finding = %+v", p.Findings[0])
	}
}

func TestDestroyDeletesTheWorker(t *testing.T) {
	s := parse(t, `{"name": "api", "services": {"web": {"kind": "server", "entry": "a.ts"}}}`)
	env := newEnv()
	env.OutDir = t.TempDir()
	var got []string
	env.Exec = func(_ context.Context, _ adapter.ExecOptions, name string, args ...string) error {
		got = append([]string{name}, args...)
		return nil
	}

	summary, err := New().DestroySummary(s, adapter.DestroyOptions{})
	if err != nil || len(summary) != 2 || !strings.Contains(summary[0], `"api"`) {
		t.Errorf("summary = %q, %v", summary, err)
	}
	result, err := New().Destroy(context.Background(), s, env, adapter.DestroyOptions{})
	if err != nil || !result.OK {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if want := []string{"npx", "wrangler", "delete", "--name", "api"}; !slices.Equal(got, want) {
		t.Errorf("exec = %q, want %q", got, want)
	}

	if _, err := New().DestroySummary(s, adapter.DestroyOptions{Volumes: true}); err == nil {
		t.Error("--volumes should be refused on cloudflare")
	}
}

// npm before 10 prefixed its messages with "npm ERR!", and Node.js 18 ships
// one; a wrangler that isn't downloaded yet must read the same there.
func TestPreflightReadsOlderNpmErrors(t *testing.T) {
	f := &exectest.Fake{Answers: map[string]exectest.Answer{
		"node --version": {Stdout: "v18.17.0\n"},
		"npx --no-install wrangler --version": {Err: errors.New("exit status 1"),
			Stderr: "npm ERR! canceled due to missing packages and no YES option: [\"wrangler@4.149.0\"]\n\nnpm ERR! A complete log of this run can be found in: /x/_logs/1-debug-0.log\n"},
	}}
	findings := preflight(context.Background(), f.Env(t.TempDir()), false)
	if len(findings) != 1 || findings[0].Code != "CF_PREFLIGHT_WRANGLER" || findings[0].Level != adapter.Info {
		t.Errorf("findings = %+v", findings)
	}
	for stderr, want := range map[string]string{
		"npm error code E404\nnpm error 404 Not Found - GET https://registry.npmjs.org/x\n":                             "code E404",
		"npm ERR! canceled due to missing packages\nnpm ERR! A complete log of this run can be found in: /x\n":          "canceled due to missing packages",
		"Error: Cannot find module 'undici'\n    at Module._resolveFilename\n    at Module._load\n":                     "Error: Cannot find module 'undici'",
		"node:internal/modules/cjs/loader:1228\n  throw err;\n  ^\n\nTypeError [ERR_INVALID_ARG_TYPE]: bad\n    at f\n": "TypeError [ERR_INVALID_ARG_TYPE]: bad",
		"something else went wrong\n": "something else went wrong",
		"":                            "",
	} {
		if got := npxError(stderr); got != want {
			t.Errorf("npxError(%q) = %q, want %q", stderr, got, want)
		}
	}
}
