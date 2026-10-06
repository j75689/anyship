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
	env   []string
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
			"gcloud config get-value account":        "dev@example.com",
			"gcloud projects describe my-project":    "123456789",
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
	c := call{name: name, args: args, env: opts.Env}
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

// Memory and cpu are always passed: the spec's, or Cloud Run's defaults when
// the spec names none, so a value taken out of the spec is taken off the
// service.
func TestPlanMemoryAndCPU(t *testing.T) {
	for size, want := range map[string]string{
		``:                             "--memory 512Mi --cpu 1",
		`"memory": "4GB",`:             "--memory 4Gi --cpu 1",
		`"memory": "1536MB",`:          "--memory 1536Mi --cpu 1",
		`"cpu": 2,`:                    "--memory 512Mi --cpu 2",
		`"memory": "16GB", "cpu": 4,`:  "--memory 16Gi --cpu 4",
		`"memory": "32GB", "cpu": 8,`:  "--memory 32Gi --cpu 8",
		`"memory": "128MB", "cpu": 1,`: "--memory 128Mi --cpu 1",
	} {
		s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27", `+size+` "ports": [{"port": 80}]}}, `+target+`}`)
		env, _ := newEnv(t, t.TempDir(), nil)
		p := plan(t, s, env)
		if errs := codes(p, adapter.Error); len(errs) > 0 {
			t.Errorf("%s: unexpected errors: %v", size, p.Findings)
			continue
		}
		data := p.Data.(*planData)
		if args := strings.Join(deployArgs(data, data.services[0], "nginx:1.27"), " "); !strings.Contains(args, want) {
			t.Errorf("%s: deploy args %q lack %q", size, args, want)
		}
	}

	for size, want := range map[string][2]string{
		`"cpu": 0.5,`:                 {"GCP_FRACTIONAL_CPU", "Set spec.targets.gcp.services.web.concurrency to 1, or use cpu: 1."},
		`"cpu": 3,`:                   {"GCP_CPU", ""},
		`"cpu": 16,`:                  {"GCP_CPU", ""},
		`"memory": "8GB",`:            {"GCP_MEMORY", "Set cpu: 2, or memory between 128MB and 4GB."},
		`"memory": "20GB", "cpu": 2,`: {"GCP_MEMORY", "Set cpu: 6, or memory between 128MB and 8GB."},
		`"cpu": 4,`:                   {"GCP_MEMORY", "Set cpu: 1, or memory between 2GB and 16GB."},
		`"memory": "64MB",`:           {"GCP_MEMORY", "Cloud Run instances have 128MB to 32GB of memory."},
		`"memory": "64GB", "cpu": 8,`: {"GCP_MEMORY", "Cloud Run instances have 128MB to 32GB of memory."},
	} {
		s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27", `+size+` "ports": [{"port": 80}]}}, `+target+`}`)
		env, _ := newEnv(t, t.TempDir(), nil)
		var found bool
		for _, f := range plan(t, s, env).Findings {
			if f.Level == adapter.Error {
				found = true
				if f.Code != want[0] || f.Hint != want[1] || f.Service != "web" {
					t.Errorf("%s: finding = %+v, want %s with hint %q", size, f, want[0], want[1])
				}
			}
		}
		if !found {
			t.Errorf("%s: no error", size)
		}
	}
}

// cronSpec is one service with cron entries and the given gcp options next to
// project and region.
func cronSpec(cron, options string) string {
	return `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27", "ports": [{"port": 80}], "cron": [` + cron + `]}},
		"targets": {"gcp": {"project": "my-project", "region": "us-central1"` + options + `}}}`
}

const twoSchedules = `{"schedule": "* * * * *", "path": "/internal/tick"}, {"schedule": "0 3 * * *", "path": "/internal/sweep", "method": "GET"}`

func TestPlanCron(t *testing.T) {
	env, _ := newEnv(t, t.TempDir(), nil)
	p := plan(t, parse(t, cronSpec(twoSchedules, "")), env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", p.Findings)
	}
	var jobs []string
	for _, a := range p.Actions {
		if a.Kind == "Cloud Scheduler job" {
			jobs = append(jobs, a.Name+": "+a.Detail)
		}
	}
	want := []string{
		`anyship_shop_web_0: "* * * * *" calls POST /internal/tick on shop-web`,
		`anyship_shop_web_1: "0 3 * * *" calls GET /internal/sweep on shop-web`,
	}
	if !slices.Equal(jobs, want) {
		t.Errorf("jobs = %q, want %q", jobs, want)
	}
	note := func(p *adapter.Plan) string {
		for _, f := range p.Findings {
			if f.Code == "GCP_CRON_HTTP" {
				return f.Message
			}
		}
		return ""
	}
	if m := note(p); !strings.Contains(m, "UTC") || !strings.Contains(m, "has to check that token itself") {
		t.Errorf("note on a public service = %q", m)
	}
	private := plan(t, parse(t, cronSpec(twoSchedules, `, "private": true`)), env)
	if m := note(private); m == "" || strings.Contains(m, "check that token itself") {
		t.Errorf("note on a private service = %q", m)
	}

	// A command has nothing to run on, and neither has an entry that names
	// nothing; each is refused by its schedule.
	refused := plan(t, parse(t, cronSpec(`{"schedule": "0 * * * *", "command": "./job"}, {"schedule": "5 * * * *"}, {"schedule": "9 * * * *", "path": "/tick"}`, "")), env)
	var messages []string
	for _, f := range refused.Findings {
		if f.Code == "GCP_CRON" && f.Level == adapter.Error {
			messages = append(messages, f.Message)
		}
	}
	if len(messages) != 2 || !strings.Contains(messages[0], `"0 * * * *"`) || !strings.Contains(messages[1], `"5 * * * *"`) {
		t.Errorf("GCP_CRON errors = %q", messages)
	}
}

// schedulerEnabled is the fake project with the Cloud Scheduler API on.
func schedulerEnabled(fc *fakeCloud) {
	fc.out["gcloud services list"] += "\ncloudscheduler.googleapis.com"
}

func TestApplyCreatesAndUpdatesCronJobs(t *testing.T) {
	s := parse(t, cronSpec(twoSchedules, `, "timeout": "10m", "serviceAccount": "runner@my-project.iam.gserviceaccount.com"`))
	env, fc := newEnv(t, t.TempDir(), nil)
	schedulerEnabled(fc)
	// The second entry's job is new; the first one's exists already.
	fc.fail = []string{"gcloud scheduler jobs describe anyship_shop_web_1"}
	res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil || !res.OK {
		t.Fatalf("apply: %v %+v", err, res)
	}
	const url = "https://shop-web-abc.a.run.app"
	update := fc.find("gcloud scheduler jobs update http anyship_shop_web_0")
	if update == nil {
		t.Fatalf("the existing job was not updated: %v", fc.calls)
	}
	for _, want := range []string{
		"--location us-central1 --schedule * * * * * --time-zone Etc/UTC",
		"--uri " + url + "/internal/tick --http-method post",
		"--attempt-deadline 600s",
		"--oidc-service-account-email runner@my-project.iam.gserviceaccount.com --oidc-token-audience " + url,
		"--clear-headers --clear-message-body",
	} {
		if !strings.Contains(update.line(), want) {
			t.Errorf("update %q lacks %q", update.line(), want)
		}
	}
	create := fc.find("gcloud scheduler jobs create http anyship_shop_web_1")
	if create == nil {
		t.Fatalf("the new job was not created: %v", fc.calls)
	}
	if line := create.line(); !strings.Contains(line, "--uri "+url+"/internal/sweep --http-method get") || strings.Contains(line, "--clear-headers") {
		t.Errorf("create = %q", line)
	}
	// A public service takes the call as it is; nothing to grant.
	if grant := fc.find("gcloud run services add-iam-policy-binding"); grant != nil {
		t.Errorf("unexpected grant on a public service: %s", grant.line())
	}
	if !slices.ContainsFunc(res.Messages, func(m string) bool { return strings.Contains(m, "2 cron schedule(s)") }) {
		t.Errorf("messages = %q", res.Messages)
	}
}

// A service that asks for authentication lets the jobs' account in, and the
// project's default account stands in when the spec names none.
func TestApplyLetsCronCallAPrivateService(t *testing.T) {
	s := parse(t, cronSpec(`{"schedule": "* * * * *", "path": "/tick"}`, `, "private": true`))
	env, fc := newEnv(t, t.TempDir(), nil)
	schedulerEnabled(fc)
	res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil || !res.OK {
		t.Fatalf("apply: %v %+v", err, res)
	}
	const account = "123456789-compute@developer.gserviceaccount.com"
	grant := fc.find("gcloud run services add-iam-policy-binding shop-web")
	if grant == nil || !slices.Contains(grant.args, "serviceAccount:"+account) || !slices.Contains(grant.args, "roles/run.invoker") {
		t.Errorf("grant = %+v", grant)
	}
	if job := fc.find("gcloud scheduler jobs update http anyship_shop_web_0"); job == nil || !strings.Contains(job.line(), "--oidc-service-account-email "+account) {
		t.Errorf("job = %+v", job)
	}
}

// An entry taken out of the spec stops firing: its job is deleted, and jobs
// that belong to another spec or to nobody are left alone.
func TestApplyRemovesStaleCronJobs(t *testing.T) {
	deleted := func(fc *fakeCloud) []string {
		var ids []string
		for _, c := range fc.calls {
			if strings.HasPrefix(c.line(), "gcloud scheduler jobs delete ") {
				ids = append(ids, c.args[3])
			}
		}
		return ids
	}
	const existing = "anyship_shop_web_0\nanyship_shop_web_1\nanyship_shop_api_0\nanyship_shop-admin_web_0\nnightly-backup"

	s := parse(t, cronSpec(`{"schedule": "* * * * *", "path": "/tick"}`, ""))
	env, fc := newEnv(t, t.TempDir(), nil)
	schedulerEnabled(fc)
	fc.out["gcloud scheduler jobs list"] = existing
	if res, err := New().Apply(context.Background(), plan(t, s, env), s, env); err != nil || !res.OK {
		t.Fatalf("apply: %v %+v", err, res)
	}
	if got, want := deleted(fc), []string{"anyship_shop_web_1", "anyship_shop_api_0"}; !slices.Equal(got, want) {
		t.Errorf("deleted = %q, want %q", got, want)
	}

	// No cron left in the spec: the jobs go, as long as the API is there to ask.
	none := parse(t, cronSpec("", ""))
	env, fc = newEnv(t, t.TempDir(), nil)
	schedulerEnabled(fc)
	fc.out["gcloud scheduler jobs list"] = existing
	if res, err := New().Apply(context.Background(), plan(t, none, env), none, env); err != nil || !res.OK {
		t.Fatalf("apply: %v %+v", err, res)
	}
	if got, want := deleted(fc), []string{"anyship_shop_web_0", "anyship_shop_web_1", "anyship_shop_api_0"}; !slices.Equal(got, want) {
		t.Errorf("deleted = %q, want %q", got, want)
	}

	env, fc = newEnv(t, t.TempDir(), nil)
	if res, err := New().Apply(context.Background(), plan(t, none, env), none, env); err != nil || !res.OK {
		t.Fatalf("apply: %v %+v", err, res)
	}
	if call := fc.find("gcloud scheduler"); call != nil {
		t.Errorf("a project without the API was asked for jobs: %s", call.line())
	}

	// A dropped entry stops firing as soon as its service is deployed,
	// even when a later service's deploy fails.
	two := parse(t, `{"name": "shop", "services": {
		"api": {"kind": "server", "image": "api:1", "ports": [{"port": 80}], "dependsOn": ["web"]},
		"web": {"kind": "server", "image": "nginx:1.27", "ports": [{"port": 80}], "cron": [{"schedule": "* * * * *", "path": "/tick"}]}},
		"targets": {"gcp": {"project": "my-project", "region": "us-central1"}}}`)
	env, fc = newEnv(t, t.TempDir(), nil)
	schedulerEnabled(fc)
	fc.out["gcloud scheduler jobs list"] = existing
	fc.fail = []string{"gcloud run deploy shop-api"}
	if res, err := New().Apply(context.Background(), plan(t, two, env), two, env); err != nil || res.OK {
		t.Fatalf("apply: %v %+v", err, res)
	}
	if got, want := deleted(fc), []string{"anyship_shop_web_1"}; !slices.Equal(got, want) {
		t.Errorf("deleted = %q, want %q", got, want)
	}
}

func TestApplyCronNeedsTheSchedulerAPI(t *testing.T) {
	s := parse(t, cronSpec(`{"schedule": "* * * * *", "path": "/tick"}`, ""))
	env, fc := newEnv(t, t.TempDir(), nil)
	res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil || res.OK {
		t.Fatalf("apply: %v %+v", err, res)
	}
	if got := codesOf(res.Findings, adapter.Error); !slices.Equal(got, []string{"GCP_PREFLIGHT_API"}) || !strings.Contains(res.Findings[0].Hint, "gcloud services enable cloudscheduler.googleapis.com") {
		t.Errorf("findings = %+v", res.Findings)
	}
	if fc.find("gcloud run deploy") != nil {
		t.Error("deployed without the Cloud Scheduler API")
	}
}

func codesOf(findings []adapter.Finding, level adapter.Level) []string {
	var out []string
	for _, f := range findings {
		if f.Level == level {
			out = append(out, f.Code)
		}
	}
	return out
}

func TestDestroyRemovesCronJobs(t *testing.T) {
	s := parse(t, cronSpec(`{"schedule": "* * * * *", "path": "/tick"}`, ""))
	env, fc := newEnv(t, t.TempDir(), nil)
	fc.out["gcloud run services list"] = `[{"metadata": {"name": "shop-web"}}]`
	fc.out["gcloud scheduler jobs list"] = "anyship_shop_web_0\nanyship_shop_web_4\nanyship_shop-admin_web_0"
	lines, err := New().DestroySummary(s, adapter.DestroyOptions{})
	if err != nil || !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "Cloud Scheduler jobs anyship_shop_*") }) {
		t.Errorf("summary = %q, %v", lines, err)
	}
	res, err := New().Destroy(context.Background(), s, env, adapter.DestroyOptions{})
	if err != nil || !res.OK {
		t.Fatalf("destroy: %v %+v", err, res)
	}
	for id, want := range map[string]bool{"anyship_shop_web_0": true, "anyship_shop_web_4": true, "anyship_shop-admin_web_0": false} {
		if got := fc.find("gcloud scheduler jobs delete "+id+" ") != nil; got != want {
			t.Errorf("deleted %s = %v, want %v", id, got, want)
		}
	}

	// The list fails where the API is off. With cron in the spec that is
	// worth a line; without, there was nothing to look for.
	env, fc = newEnv(t, t.TempDir(), nil)
	fc.out["gcloud run services list"] = `[{"metadata": {"name": "shop-web"}}]`
	fc.fail = []string{"gcloud scheduler jobs list"}
	res, err = New().Destroy(context.Background(), s, env, adapter.DestroyOptions{})
	if err != nil || !res.OK || !slices.ContainsFunc(res.Messages, func(m string) bool { return strings.Contains(m, "Cloud Scheduler jobs were not removed") }) {
		t.Errorf("destroy with a failing list: %v %+v", err, res)
	}
	plain := parse(t, cronSpec("", ""))
	res, err = New().Destroy(context.Background(), plain, env, adapter.DestroyOptions{})
	if err != nil || !res.OK || len(res.Messages) != 1 {
		t.Errorf("destroy without cron: %v %+v", err, res)
	}
}

// policyWith is a secret's IAM policy in gcloud's JSON, with secretAccessor
// for the members, and an unrelated role that must be left alone.
func policyWith(members ...string) string {
	quoted := make([]string, len(members))
	for i, m := range members {
		quoted[i] = `"` + m + `"`
	}
	return `{"bindings": [{"role": "roles/secretmanager.secretAccessor", "members": [` + strings.Join(quoted, ", ") + `]},
		{"role": "roles/secretmanager.viewer", "members": ["serviceAccount:old@my-project.iam.gserviceaccount.com"]}], "etag": "x"}`
}

// Access to a secret follows the spec: service accounts that may read it
// without a service listing it, or a place in secretReaders, lose that
// right. Users, groups, other roles and secrets anyship didn't make are not
// touched.
func TestApplyTakesStaleSecretAccessAway(t *testing.T) {
	s := parse(t, settings(`, "services": {"render": {"serviceAccount": "render@my-project.iam.gserviceaccount.com"}},
		"secretReaders": ["reports@my-project.iam.gserviceaccount.com"]`))
	env, fc := newEnv(t, t.TempDir(), map[string]string{"SESSION": "a", "TTS_KEY": "b", "SHARED": "c"})
	const old = "serviceAccount:old@my-project.iam.gserviceaccount.com"
	fc.out["gcloud secrets describe shop-SESSION"] = "shop"
	fc.out["gcloud secrets describe shop-TTS_KEY"] = "shop"
	fc.out["gcloud secrets get-iam-policy shop-SESSION"] = policyWith(old, "serviceAccount:123456789-compute@developer.gserviceaccount.com",
		"serviceAccount:reports@my-project.iam.gserviceaccount.com", "user:dev@example.com", "deleted:serviceAccount:gone@my-project.iam.gserviceaccount.com?uid=1")
	// render's account may read TTS_KEY; the default account may not.
	fc.out["gcloud secrets get-iam-policy shop-TTS_KEY"] = policyWith("serviceAccount:render@my-project.iam.gserviceaccount.com", "serviceAccount:123456789-compute@developer.gserviceaccount.com")
	// SHARED carries no anyship label: somebody else's secret, left alone.
	fc.out["gcloud secrets get-iam-policy shop-SHARED"] = policyWith(old)

	res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil || !res.OK {
		t.Fatalf("apply: %v %+v", err, res)
	}
	var revoked []string
	for _, c := range fc.calls {
		if c.name == "gcloud" && len(c.args) > 2 && c.args[0] == "secrets" && c.args[1] == "remove-iam-policy-binding" {
			revoked = append(revoked, c.args[2]+" "+c.args[slices.Index(c.args, "--member")+1]+" "+c.args[slices.Index(c.args, "--role")+1])
		}
	}
	slices.Sort(revoked)
	want := []string{
		"shop-SESSION " + old + " roles/secretmanager.secretAccessor",
		"shop-TTS_KEY serviceAccount:123456789-compute@developer.gserviceaccount.com roles/secretmanager.secretAccessor",
	}
	if !slices.Equal(revoked, want) {
		t.Errorf("revoked = %q, want %q", revoked, want)
	}

	// A dry run names them and changes nothing.
	env, fc = newEnv(t, t.TempDir(), map[string]string{"SESSION": "a", "TTS_KEY": "b", "SHARED": "c"})
	fc.out["gcloud secrets describe shop-SESSION"] = "shop"
	fc.out["gcloud secrets get-iam-policy shop-SESSION"] = policyWith(old)
	env.DryRun = true
	res, err = New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil || !res.OK {
		t.Fatalf("dry run: %v %+v", err, res)
	}
	if !slices.ContainsFunc(res.Messages, func(m string) bool {
		return strings.Contains(m, "Would take the right to read secret shop-SESSION away from old@my-project.iam.gserviceaccount.com")
	}) {
		t.Errorf("dry-run messages = %q", res.Messages)
	}
	if fc.find("gcloud secrets remove-iam-policy-binding") != nil || fc.find("gcloud secrets add-iam-policy-binding") != nil {
		t.Error("a dry run changed a binding")
	}
}

func TestPlanHealthCheck(t *testing.T) {
	tests := map[string]struct {
		healthCheck string
		level       adapter.Level
		code        string
		// probe is the value of --startup-probe; empty removes the probe.
		probe string
	}{
		"path": {
			healthCheck: `{"path": "/health"}`,
			level:       adapter.Info, code: "GCP_HEALTH_CHECK",
			probe: "httpGet.path=/health,periodSeconds=10,timeoutSeconds=5,failureThreshold=24",
		},
		"path with a comma": {
			healthCheck: `{"path": "/health?checks=db,cache"}`,
			level:       adapter.Info, code: "GCP_HEALTH_CHECK",
			probe: "^|^httpGet.path=/health?checks=db,cache|periodSeconds=10|timeoutSeconds=5|failureThreshold=24",
		},
		"path wins over command": {
			healthCheck: `{"path": "/health", "command": "curl -f localhost"}`,
			level:       adapter.Info, code: "GCP_HEALTH_CHECK",
			probe: "httpGet.path=/health,periodSeconds=10,timeoutSeconds=5,failureThreshold=24",
		},
		"command only": {
			healthCheck: `{"command": "curl -f localhost"}`,
			level:       adapter.Warning, code: "GCP_HEALTH_COMMAND_IGNORED",
		},
		"path without a slash": {
			healthCheck: `{"path": "health"}`,
			level:       adapter.Error, code: "GCP_BAD_HEALTH_PATH",
		},
		"none": {healthCheck: `null`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := parse(t, `{"name": "shop",
				"services": {"web": {"kind": "server", "image": "nginx:1.27", "ports": [{"port": 80}],
					"healthCheck": `+tc.healthCheck+`}}, `+target+`}`)
			env, _ := newEnv(t, t.TempDir(), nil)
			p := plan(t, s, env)
			if tc.code != "" && !slices.Contains(codes(p, tc.level), tc.code) {
				t.Errorf("missing %s %s in %v", tc.level, tc.code, p.Findings)
			}
			if tc.level == adapter.Error {
				return
			}
			if errs := codes(p, adapter.Error); len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if tc.level != adapter.Warning {
				if warnings := codes(p, adapter.Warning); len(warnings) > 0 {
					t.Errorf("unexpected warnings: %v", warnings)
				}
			}
			data := p.Data.(*planData)
			args := deployArgs(data, data.services[0], "nginx:1.27")
			// The flag is always passed, so a path taken out of the spec
			// takes the probe off the service.
			i := slices.Index(args, "--startup-probe")
			if i < 0 || i+1 >= len(args) {
				t.Fatalf("deploy args %q lack --startup-probe", args)
			}
			if args[i+1] != tc.probe {
				t.Errorf("--startup-probe = %q, want %q", args[i+1], tc.probe)
			}
		})
	}
}

func TestPlanImageRegistry(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("a", 64)
	for image, pulls := range map[string]bool{
		"nginx:1.27":                                true,
		"acme/shop:v2":                              true,
		"docker.io/acme/shop:v2":                    true,
		"gcr.io/my-project/shop:v2":                 true,
		"asia.gcr.io/my-project/shop:v2":            true,
		"us-docker.pkg.dev/my-project/apps/shop:v2": true,
		"us-central1-docker.pkg.dev/my-project/ghcr/acme/shop" + digest: true,
		"index.docker.io/library/nginx:1.27":                            true,
		"ghcr.io/acme/shop" + digest:                                    true,
		"registry.hub.docker.com/library/nginx:1.27":                    false,
		"quay.io/acme/shop:v2":                                          false,
		"public.ecr.aws/acme/shop:v2":                                   false,
		"registry.example.com:5000/shop:v2":                             false,
		"evil-gcr.io/acme/shop:v2":                                      false,
	} {
		s := parse(t, `{"name": "shop",
			"services": {"web": {"kind": "server", "image": "`+image+`", "ports": [{"port": 80}]}}, `+target+`}`)
		env, _ := newEnv(t, t.TempDir(), nil)
		p := plan(t, s, env)
		if refused := slices.Contains(codes(p, adapter.Error), "GCP_UNPULLABLE_IMAGE"); refused == pulls {
			t.Errorf("image %s: refused = %v, want %v (%v)", image, refused, !pulls, p.Findings)
		}
	}

	// Cloud Run pulls public ghcr.io images itself and can't pull private
	// ones; plan can't tell which, so it warns and names the way around.
	for image, code := range map[string]string{"ghcr.io/acme/shop:v2": "GCP_GHCR_PUBLIC_ONLY", "quay.io/acme/shop:v2": "GCP_UNPULLABLE_IMAGE"} {
		s := parse(t, `{"name": "shop",
			"services": {"web": {"kind": "server", "image": "`+image+`", "ports": [{"port": 80}]}}, `+target+`}`)
		env, _ := newEnv(t, t.TempDir(), nil)
		i := slices.IndexFunc(plan(t, s, env).Findings, func(f adapter.Finding) bool { return f.Code == code })
		if i < 0 {
			t.Errorf("%s: no %s finding", image, code)
			continue
		}
		if f := plan(t, s, env).Findings[i]; !strings.Contains(f.Hint, "us-central1-docker.pkg.dev/my-project/<remote repository>/acme/shop:v2") {
			t.Errorf("%s: hint %q doesn't name the remote repository path", image, f.Hint)
		}
	}
}

// An internal port requires a token and keeps internal ingress, as before,
// unless the target says ingress "all"; the spec's other services get the
// permission to call either way.
func TestPlanInternalPort(t *testing.T) {
	spec := func(options string) *spec.Spec {
		return parse(t, `{"name": "shop",
			"services": {
				"web": {"kind": "server", "image": "nginx:1.27", "ports": [{"port": 80}], "env": {"API": "${services.api.url}/v1"}},
				"api": {"kind": "server", "image": "nginx:1.27", "ports": [{"port": 8080, "exposure": "internal"}]}},
			"targets": {"gcp": {"project": "my-project", "region": "us-central1"`+options+`}}}`)
	}
	env, _ := newEnv(t, t.TempDir(), nil)
	closed := plan(t, spec(``), env)
	i := slices.IndexFunc(closed.Findings, func(f adapter.Finding) bool { return f.Code == "GCP_INTERNAL_CALLERS" })
	if i < 0 || closed.Findings[i].Level != adapter.Warning || !strings.Contains(closed.Findings[i].Hint, "services.api.ingress: all") {
		t.Errorf("GCP_INTERNAL_CALLERS finding = %+v", closed.Findings)
	}
	data := closed.Data.(*planData)
	if args := strings.Join(deployArgs(data, data.services[0], "nginx:1.27"), " "); !strings.Contains(args, "--ingress internal --no-allow-unauthenticated") {
		t.Errorf("api deploy args %q: internal ingress was given up", args)
	}
	if errs := codes(plan(t, spec(`, "services": {"web": {"ingress": "all"}}`), env), adapter.Error); !slices.Equal(errs, []string{"GCP_INGRESS"}) {
		t.Errorf("ingress on a public service: errors = %v, want GCP_INGRESS", errs)
	}

	s := spec(`, "services": {"api": {"ingress": "all"}}`)
	env, fc := newEnv(t, t.TempDir(), nil)
	p := plan(t, s, env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	i = slices.IndexFunc(p.Findings, func(f adapter.Finding) bool { return f.Code == "GCP_INTERNAL" })
	if i < 0 || p.Findings[i].Level != adapter.Info || p.Findings[i].Service != "api" || !strings.Contains(p.Findings[i].Hint, "${services.api.url}") {
		t.Errorf("GCP_INTERNAL finding = %+v", p.Findings)
	}
	data = p.Data.(*planData)
	args := strings.Join(deployArgs(data, data.services[0], "nginx:1.27"), " ")
	if !strings.Contains(args, "--ingress all --no-allow-unauthenticated") {
		t.Errorf("api deploy args %q: not guarded by identity", args)
	}
	// Before preflight the URL carries a placeholder for the project number.
	web := strings.Join(deployArgs(data, data.services[1], "nginx:1.27"), " ")
	if want := "--set-env-vars API=https://shop-api-<project number>.us-central1.run.app/v1"; !strings.Contains(web, want) {
		t.Errorf("web deploy args %q lack %q", web, want)
	}

	res, err := New().Apply(context.Background(), p, s, env)
	if err != nil || !res.OK {
		t.Fatalf("apply: %v %+v", err, res)
	}
	if deploy := fc.find("gcloud run deploy shop-web"); deploy == nil || !strings.Contains(deploy.line(), "API=https://shop-api-123456789.us-central1.run.app/v1") {
		t.Errorf("web wasn't given api's URL: %+v", deploy)
	}
	grant := fc.find("gcloud run services add-iam-policy-binding shop-api")
	if grant == nil || !slices.Contains(grant.args, "serviceAccount:123456789-compute@developer.gserviceaccount.com") || !slices.Contains(grant.args, "roles/run.invoker") {
		t.Errorf("web's account can't call api: %+v", grant)
	}
	if fc.find("gcloud run services add-iam-policy-binding shop-web") != nil {
		t.Error("a public service was given a caller binding")
	}
}

// Each internal service lets in the accounts of the other services, each
// once, and not its own.
func TestApplyAllowsCallersPerAccount(t *testing.T) {
	s := parse(t, `{"name": "shop",
		"services": {
			"web": {"kind": "server", "image": "nginx:1.27", "ports": [{"port": 80}]},
			"worker": {"kind": "server", "image": "nginx:1.27", "ports": [{"port": 80}]},
			"api": {"kind": "server", "image": "nginx:1.27", "ports": [{"port": 8080, "exposure": "internal"}]}},
		"targets": {"gcp": {"project": "my-project", "region": "us-central1",
			"services": {"api": {"serviceAccount": "apisvc@my-project.iam.gserviceaccount.com"}, "worker": {"serviceAccount": "worker@my-project.iam.gserviceaccount.com"}}}}}`)
	env, fc := newEnv(t, t.TempDir(), nil)
	res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil || !res.OK {
		t.Fatalf("apply: %v %+v", err, res)
	}
	var members []string
	for _, c := range fc.calls {
		if strings.HasPrefix(c.line(), "gcloud run services add-iam-policy-binding shop-api") {
			members = append(members, c.args[slices.Index(c.args, "--member")+1])
		}
	}
	slices.Sort(members)
	if want := []string{"serviceAccount:123456789-compute@developer.gserviceaccount.com", "serviceAccount:worker@my-project.iam.gserviceaccount.com"}; !slices.Equal(members, want) {
		t.Errorf("members = %q, want %q", members, want)
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
		"bad timeout":   `{"gcp": {"project": "my-project", "region": "us-central1", "timeout": "10"}}`,
		"long timeout":  `{"gcp": {"project": "my-project", "region": "us-central1", "timeout": "2h"}}`,
		"part seconds":  `{"gcp": {"project": "my-project", "region": "us-central1", "timeout": "1.5s"}}`,
		"zero timeout":  `{"gcp": {"project": "my-project", "region": "us-central1", "timeout": "0s"}}`,
		"negative":      `{"gcp": {"project": "my-project", "region": "us-central1", "timeout": "-5m"}}`,
		"over an hour":  `{"gcp": {"project": "my-project", "region": "us-central1", "timeout": "1h1s"}}`,
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
	grant := fc.find("gcloud secrets add-iam-policy-binding shop-SESSION")
	if grant == nil || !slices.Contains(grant.args, "serviceAccount:123456789-compute@developer.gserviceaccount.com") ||
		!slices.Contains(grant.args, "roles/secretmanager.secretAccessor") {
		t.Errorf("secret access not granted to the runtime account: %+v", grant)
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

func TestApplyWithServiceAccountAndConfiguration(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx", "secrets": ["TOKEN"]}},
		"secrets": {"TOKEN": {}}, "targets": {"gcp": {"project": "my-project", "region": "us-central1",
			"configuration": "anyship-e2e", "serviceAccount": "runner@my-project.iam.gserviceaccount.com"}}}`)
	env, fc := newEnv(t, t.TempDir(), map[string]string{"TOKEN": "s3cret"})
	res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil || !res.OK {
		t.Fatalf("apply: %v %+v", err, res)
	}
	for _, c := range fc.calls {
		if c.name == "gcloud" && !slices.Contains(c.env, "CLOUDSDK_ACTIVE_CONFIG_NAME=anyship-e2e") {
			t.Errorf("gcloud ran outside the configuration: %s", c.line())
		}
	}
	if grant := fc.find("gcloud secrets add-iam-policy-binding shop-TOKEN"); grant == nil || !slices.Contains(grant.args, "serviceAccount:runner@my-project.iam.gserviceaccount.com") {
		t.Errorf("grant = %+v", grant)
	}
	if fc.find("gcloud run deploy shop-web") == nil || !strings.Contains(fc.find("gcloud run deploy shop-web").line(), "--service-account runner@my-project.iam.gserviceaccount.com") {
		t.Errorf("deploy doesn't run as the service account: %v", fc.calls)
	}
}

func TestPreflightNamesTheAccount(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}}, `+target+`}`)
	env, fc := newEnv(t, t.TempDir(), nil)
	fc.fail = []string{"gcloud projects describe"}
	res, _ := New().Apply(context.Background(), plan(t, s, env), s, env)
	if res.OK || res.Findings[0].Code != "GCP_PREFLIGHT_PROJECT" || !strings.Contains(res.Findings[0].Message, "dev@example.com can't read project my-project") {
		t.Errorf("findings = %+v", res.Findings)
	}
}

func TestGcloudError(t *testing.T) {
	stderr := "Encryption: Google-managed key\nERROR: (gcloud.artifacts.repositories.describe) NOT_FOUND: Requested entity was not found.\n"
	if got := gcloudError(stderr); got != "(gcloud.artifacts.repositories.describe) NOT_FOUND: Requested entity was not found." {
		t.Errorf("gcloudError = %q", got)
	}
	if got := gcloudError("network is unreachable\n"); got != "network is unreachable" {
		t.Errorf("gcloudError = %q", got)
	}
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

func TestTimeout(t *testing.T) {
	for timeout, want := range map[string]string{"10m": "--timeout 600", "1h": "--timeout 3600", "1s": "--timeout 1", "1m30s": "--timeout 90"} {
		s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}},
			"targets": {"gcp": {"project": "my-project", "region": "us-central1", "timeout": "`+timeout+`"}}}`)
		env, _ := newEnv(t, t.TempDir(), nil)
		p := plan(t, s, env)
		if errs := codes(p, adapter.Error); len(errs) > 0 {
			t.Fatalf("timeout %q: errors %v", timeout, errs)
		}
		data := p.Data.(*planData)
		args := strings.Join(deployArgs(data, data.services[0], "nginx"), " ")
		if !strings.Contains(args, want) {
			t.Errorf("timeout %q: deploy args %q", timeout, args)
		}
	}
}

// settings is a two-service spec with the given gcp options next to project
// and region.
func settings(options string) string {
	return `{"name": "shop",
		"services": {
			"web": {"kind": "server", "image": "nginx:1.27", "ports": [{"port": 80}], "secrets": ["SESSION", "SHARED"]},
			"render": {"kind": "server", "image": "nginx:1.27", "ports": [{"port": 80}], "memory": "4GB", "cpu": 2, "replicas": 2, "secrets": ["TTS_KEY", "SHARED"]}},
		"secrets": {"SESSION": {}, "TTS_KEY": {}, "SHARED": {}},
		"targets": {"gcp": {"project": "my-project", "region": "us-central1"` + options + `}}}`
}

// Every setting is passed on every deploy: the spec's value, or the word or
// number that puts Cloud Run's default back.
func TestPlanServiceSettings(t *testing.T) {
	s := parse(t, settings(`, "timeout": "10m", "serviceAccount": "runner@my-project.iam.gserviceaccount.com",
		"services": {"render": {"maxInstances": 3, "concurrency": 1, "timeout": "15m", "executionEnvironment": "gen2",
			"serviceAccount": "render@my-project.iam.gserviceaccount.com"}}`))
	env, _ := newEnv(t, t.TempDir(), nil)
	p := plan(t, s, env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", p.Findings)
	}
	data := p.Data.(*planData)
	want := map[string]string{
		"render": "--min-instances 2 --max-instances 3 --concurrency 1 --timeout 900 --execution-environment gen2 --service-account render@my-project.iam.gserviceaccount.com",
		"web":    "--min-instances default --max-instances default --concurrency default --timeout 600 --service-account runner@my-project.iam.gserviceaccount.com",
	}
	for _, sv := range data.services {
		if args := strings.Join(deployArgs(data, sv, sv.image), " "); !strings.Contains(args, want[sv.name]) {
			t.Errorf("%s: deploy args %q lack %q", sv.name, args, want[sv.name])
		}
	}

	// Nothing set: Cloud Run's defaults, and no account until Apply knows
	// the project's default one. The plan's copy of the command shows where
	// it will go.
	bare := plan(t, parse(t, settings(``)), env)
	data = bare.Data.(*planData)
	args := strings.Join(deployArgs(data, data.services[1], "nginx:1.27"), " ")
	if !strings.Contains(args, "--min-instances default --max-instances default --concurrency default --timeout 300 --startup-probe") {
		t.Errorf("deploy args %q don't reset the settings", args)
	}
	if strings.Contains(args, "--execution-environment") || strings.Contains(args, "--service-account") {
		t.Errorf("deploy args %q name an environment or an account the spec doesn't", args)
	}
	i := slices.IndexFunc(bare.Files, func(f adapter.File) bool { return filepath.Base(f.Path) == "web.gcloud.txt" })
	if i < 0 || !strings.Contains(string(bare.Files[i].Contents), "--service-account '<project number>-compute@developer.gserviceaccount.com'") {
		t.Errorf("the plan's deploy command doesn't show the default account: %s", bare.Files)
	}
}

func TestPlanFractionalCPU(t *testing.T) {
	spec := func(options string) *spec.Spec {
		return parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27", "ports": [{"port": 80}], "cpu": 0.5, "memory": "1GB"}},
			"targets": {"gcp": {"project": "my-project", "region": "us-central1", "services": {"web": `+options+`}}}}`)
	}
	env, _ := newEnv(t, t.TempDir(), nil)
	p := plan(t, spec(`{"concurrency": 1}`), env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", p.Findings)
	}
	data := p.Data.(*planData)
	args := strings.Join(deployArgs(data, data.services[0], "nginx:1.27"), " ")
	if want := "--memory 1Gi --cpu 0.5"; !strings.Contains(args, want) {
		t.Errorf("deploy args %q lack %q", args, want)
	}
	if want := "--concurrency 1 --timeout 300 --execution-environment gen1 --cpu-throttling"; !strings.Contains(args, want) {
		t.Errorf("deploy args %q lack %q", args, want)
	}

	for options, want := range map[string]string{
		`{}`:                 "GCP_FRACTIONAL_CPU",
		`{"concurrency": 2}`: "GCP_FRACTIONAL_CPU",
		`{"concurrency": 1, "executionEnvironment": "gen2"}`: "GCP_FRACTIONAL_CPU",
	} {
		if errs := codes(plan(t, spec(options), env), adapter.Error); !slices.Equal(errs, []string{want}) {
			t.Errorf("%s: errors = %v, want %s", options, errs, want)
		}
	}
	// Half a CPU carries at most 1GB, and a twentieth of one isn't offered.
	for size, want := range map[string]string{`"cpu": 0.5, "memory": "2GB"`: "GCP_MEMORY", `"cpu": 0.25, "memory": "1GB"`: "GCP_MEMORY", `"cpu": 0.05`: "GCP_CPU"} {
		s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27", "ports": [{"port": 80}], `+size+`}},
			"targets": {"gcp": {"project": "my-project", "region": "us-central1", "services": {"web": {"concurrency": 1}}}}}`)
		if errs := codes(plan(t, s, env), adapter.Error); !slices.Equal(errs, []string{want}) {
			t.Errorf("%s: errors = %v, want %s", size, errs, want)
		}
	}
}

func TestPlanRefusesBadServiceSettings(t *testing.T) {
	env, _ := newEnv(t, t.TempDir(), nil)
	for options, want := range map[string]string{
		`{"web": {"maxInstances": -1}}`:               "services.web: maxInstances -1",
		`{"web": {"concurrency": 1001}}`:              "services.web: concurrency 1001",
		`{"web": {"timeout": "2h"}}`:                  `services.web: timeout "2h"`,
		`{"web": {"timeout": 600}}`:                   "services.web.timeout",
		`{"web": {"serviceAccount": "me@gmail.com"}}`: "services.web: serviceAccount",
		`{"web": {"executionEnvironment": "gen3"}}`:   `services.web: executionEnvironment "gen3" is not gen1 or gen2`,
		`{"web": {"ingress": "public"}}`:              `services.web: ingress "public" is not internal or all`,
		`{}, "secretReaders": ["me@gmail.com"]`:       `secretReaders.0: "me@gmail.com" is not a service account email`,
		`{"web": {"maxInstance": 3}}`:                 "services.web.maxInstance",
		`{"api": {"maxInstances": 3}}`:                `spec.targets.gcp.services.api: the spec has no service named "api" (it has render, web).`,
		`{"render": {"maxInstances": 1}}`:             "maxInstances 1 is below replicas 2",
		`{"web": {"executionEnvironment": "gen2"}}`:   "",
	} {
		p := plan(t, parse(t, settings(`, "services": `+options)), env)
		var messages []string
		for _, f := range p.Findings {
			if f.Level == adapter.Error {
				messages = append(messages, f.Message)
			}
		}
		switch {
		case want == "" && len(messages) > 0:
			t.Errorf("%s: unexpected errors %q", options, messages)
		case want != "" && !slices.ContainsFunc(messages, func(m string) bool { return strings.Contains(m, want) }):
			t.Errorf("%s: errors %q lack %q", options, messages, want)
		}
	}

	small := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx:1.27", "memory": "256MB"}},
		"targets": {"gcp": {"project": "my-project", "region": "us-central1", "services": {"web": {"executionEnvironment": "gen2"}}}}}`)
	if errs := codes(plan(t, small, env), adapter.Error); !slices.Equal(errs, []string{"GCP_MEMORY"}) {
		t.Errorf("256MB on gen2: errors = %v, want GCP_MEMORY", errs)
	}
}

// Each account is let into the secrets of the services that run as it, and
// a service without an account of its own runs as the project's default one.
func TestApplyGrantsSecretsPerAccount(t *testing.T) {
	s := parse(t, settings(`, "services": {"render": {"serviceAccount": "render@my-project.iam.gserviceaccount.com"}}`))
	env, fc := newEnv(t, t.TempDir(), map[string]string{"SESSION": "a", "TTS_KEY": "b", "SHARED": "c"})
	res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil || !res.OK {
		t.Fatalf("apply: %v %+v", err, res)
	}
	var grants []string
	for _, c := range fc.calls {
		if c.name == "gcloud" && len(c.args) > 3 && c.args[0] == "secrets" && c.args[1] == "add-iam-policy-binding" {
			grants = append(grants, c.args[2]+" "+c.args[slices.Index(c.args, "--member")+1])
		}
	}
	slices.Sort(grants)
	const fallback, render = "serviceAccount:123456789-compute@developer.gserviceaccount.com", "serviceAccount:render@my-project.iam.gserviceaccount.com"
	if want := []string{"shop-SESSION " + fallback, "shop-SHARED " + fallback, "shop-SHARED " + render, "shop-TTS_KEY " + render}; !slices.Equal(grants, want) {
		t.Errorf("grants = %q, want %q", grants, want)
	}
	for service, account := range map[string]string{"shop-web": "123456789-compute@developer.gserviceaccount.com", "shop-render": "render@my-project.iam.gserviceaccount.com"} {
		if deploy := fc.find("gcloud run deploy " + service); deploy == nil || !strings.Contains(deploy.line(), "--service-account "+account) {
			t.Errorf("%s doesn't run as %s: %+v", service, account, deploy)
		}
	}
}

// gcloud takes a bare number of seconds, so people will write one.
func TestTimeoutNeedsAUnit(t *testing.T) {
	_, err := decodeOptions([]byte(`{"project": "my-project", "region": "us-central1", "timeout": 600}`))
	if err == nil || !strings.Contains(err.Error(), "needs a unit") {
		t.Errorf("error = %v, want it to ask for a unit", err)
	}
}

// The spec is YAML: the hint for a missing target block names the path in
// anyship.yaml and shows fields as they are written there, never as JSON.
func TestMissingOptionsHint(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}}}`)
	env, _ := newEnv(t, t.TempDir(), nil)
	p := plan(t, s, env)
	if len(p.Findings) != 1 || p.Findings[0].Code != "GCP_BAD_OPTIONS" {
		t.Fatalf("findings = %+v", p.Findings)
	}
	f := p.Findings[0]
	if !strings.HasPrefix(f.Message, "spec.targets."+Name+": ") {
		t.Errorf("message %q does not name the path in anyship.yaml", f.Message)
	}
	if !strings.HasPrefix(f.Hint, "Under spec.targets."+Name+" in anyship.yaml, set `") || strings.ContainsAny(f.Hint, "{}") {
		t.Errorf("hint %q", f.Hint)
	}
	if _, err := os.Stat(filepath.Join("..", "..", "docs", "targets", Name+".md")); err != nil {
		t.Errorf("the hint links to a page that does not exist: %v", err)
	}
}

// A tag that moves can deploy a stale image and still report success, so the
// plan says so; a version or a digest is taken at its word.
func TestPlanWarnsAboutMovingTags(t *testing.T) {
	for image, tag := range map[string]string{
		"nginx": "latest",
		"us-docker.pkg.dev/my-project/ghcr/acme/shop:main": "main",
		"nginx:1.27": "",
		"us-docker.pkg.dev/my-project/ghcr/acme/shop@sha256:0000000000000000000000000000000000000000000000000000000000000000": "",
	} {
		s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "`+image+`"}}, "targets": {"gcp": {"project": "my-project", "region": "us-central1"}}}`)
		env, _ := newEnv(t, t.TempDir(), nil)
		p := plan(t, s, env)
		var warning *adapter.Finding
		for i, f := range p.Findings {
			if f.Code == "GCP_MUTABLE_TAG" {
				warning = &p.Findings[i]
			}
		}
		switch {
		case tag == "" && warning != nil:
			t.Errorf("%s: unexpected warning %q", image, warning.Message)
		case tag != "" && warning == nil:
			t.Errorf("%s: no GCP_MUTABLE_TAG warning in %v", image, codes(p, adapter.Warning))
		case tag != "":
			if warning.Level != adapter.Warning || !strings.Contains(warning.Message, `the tag "`+tag+`"`) || !strings.Contains(warning.Hint, "--image web=<ref>") {
				t.Errorf("%s: %+v", image, *warning)
			}
		}
		if adapter.HasErrors(p.Findings) {
			t.Errorf("%s: a moving tag is a warning, not a refusal: %v", image, codes(p, adapter.Error))
		}
	}
}
