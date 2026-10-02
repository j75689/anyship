package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
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
	if p.Files[0].Path != "/work/app/.anyship/cloudflare/wrangler.jsonc" {
		t.Errorf("path = %s", p.Files[0].Path)
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
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
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
	if want := []string{"make", "npx wrangler deploy --config " + configPath + " --dry-run"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	written, err := os.ReadFile(configPath)
	if err != nil || !bytes.Contains(written, []byte(`"name": "site"`)) {
		t.Errorf("config not written: %v %s", err, written)
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
