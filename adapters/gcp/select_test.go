package gcp

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

// selectSpec has web, which calls search by URL; search, which web calls
// and which shares a secret with it; and reels, on its own. Each has a cron
// entry, and search and web have internal ports.
func selectSpec(t *testing.T, reels string) *spec.Spec {
	t.Helper()
	return parse(t, `{"name": "shop",
		"services": {
			"web": {"kind": "server", "image": "web:1", "ports": [{"port": 80, "exposure": "internal"}], "secrets": ["DB_URL"],
				"env": {"SEARCH": "${services.search.url}"}, "cron": [{"schedule": "* * * * *", "path": "/tick"}]},
			"search": {"kind": "server", "image": "search:1", "ports": [{"port": 8080, "exposure": "internal"}], "secrets": ["DB_URL", "SEARCH_KEY"],
				"cron": [{"schedule": "0 * * * *", "path": "/tick"}]},
			"reels": {"kind": "server", "image": "reels:1", "ports": [{"port": 80}], "cron": [{"schedule": "0 3 * * *", "path": "/sweep"}]`+reels+`}},
		"secrets": {"DB_URL": {}, "SEARCH_KEY": {}},
		"targets": {"gcp": {"project": "my-project", "region": "us-central1", "services": {
			"web": {"serviceAccount": "websvc@my-project.iam.gserviceaccount.com", "ingress": "all"},
			"search": {"serviceAccount": "searchsvc@my-project.iam.gserviceaccount.com", "ingress": "all"}}}}}`)
}

const (
	searchURL       = "https://shop-search-123456789.us-central1.run.app"
	searchLegacyURL = "https://shop-search-abc-uc.a.run.app"
	liveSearch      = "live-search@my-project.iam.gserviceaccount.com"
)

// runningSearch is `gcloud run services list` with search running as
// liveSearch, answering on urls.
func runningSearch(urls string) string {
	return `[{"metadata": {"name": "shop-search", "annotations": {"run.googleapis.com/urls": "` + strings.ReplaceAll(urls, `"`, `\"`) + `"}},
		"spec": {"template": {"spec": {"serviceAccountName": "` + liveSearch + `"}}},
		"status": {"url": "` + searchLegacyURL + `"}}]`
}

// selectEnv is a project where search runs, web is selected and DB_URL is
// one of anyship's secrets.
func selectEnv(t *testing.T) (*adapter.Env, *fakeCloud) {
	t.Helper()
	env, fc := newEnv(t, t.TempDir(), nil)
	schedulerEnabled(fc)
	env.Services = []string{"web"}
	fc.out["gcloud run services list"] = runningSearch(`["` + searchURL + `", "` + searchLegacyURL + `"]`)
	fc.out["gcloud secrets describe shop-DB_URL"] = "shop"
	return env, fc
}

func linesOf(fc *fakeCloud, prefix string) []string {
	var lines []string
	for _, c := range fc.calls {
		if strings.HasPrefix(c.line(), prefix) {
			lines = append(lines, c.line())
		}
	}
	return lines
}

func applySelected(t *testing.T, s *spec.Spec, env *adapter.Env) *adapter.Result {
	t.Helper()
	res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Criterion 1: only the selected services' Cloud Run services and cron jobs
// are deployed.
func TestApplySelectedDeploysOnlyThoseServices(t *testing.T) {
	s := selectSpec(t, "")
	env, fc := selectEnv(t)
	if res := applySelected(t, s, env); !res.OK {
		t.Fatalf("apply: %+v", res)
	}
	deploys := linesOf(fc, "gcloud run deploy ")
	if len(deploys) != 1 || !strings.HasPrefix(deploys[0], "gcloud run deploy shop-web ") {
		t.Errorf("deploys = %q, want shop-web only", deploys)
	}
	var jobs []string
	for _, verb := range []string{"create", "update"} {
		jobs = append(jobs, linesOf(fc, "gcloud scheduler jobs "+verb)...)
	}
	if len(jobs) != 1 || !strings.Contains(jobs[0], " anyship_shop_web_0 ") {
		t.Errorf("jobs = %q, want web's only", jobs)
	}
}

// Criterion 2: only the secrets the selected services list are made or kept.
func TestPlanSelectedSecretsOnly(t *testing.T) {
	s := selectSpec(t, "")
	env, fc := selectEnv(t)
	p := plan(t, s, env)
	var secrets []string
	for _, a := range p.Actions {
		if a.Kind == "Secret Manager secret" {
			secrets = append(secrets, a.Name)
		}
	}
	if !slices.Equal(secrets, []string{"shop-DB_URL"}) {
		t.Errorf("secrets = %q, want shop-DB_URL", secrets)
	}
	if res := applySelected(t, s, env); !res.OK {
		t.Fatalf("apply: %+v", res)
	}
	if lines := linesOf(fc, "gcloud secrets"); slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "SEARCH_KEY") }) {
		t.Errorf("a secret only search lists was touched: %q", lines)
	}
}

func revokedFrom(fc *fakeCloud) []string {
	var revoked []string
	for _, c := range fc.calls {
		if strings.HasPrefix(c.line(), "gcloud secrets remove-iam-policy-binding ") {
			revoked = append(revoked, c.args[2]+" "+c.args[slices.Index(c.args, "--member")+1])
		}
	}
	return revoked
}

// Criterion 3: access is granted for the selected services only, and never
// taken from an account a left-out service runs as, in the spec or on Cloud
// Run. A reader nobody runs as still loses it.
func TestApplySelectedKeepsLeftOutAccess(t *testing.T) {
	s := selectSpec(t, "")
	env, fc := selectEnv(t)
	const old = "serviceAccount:old@my-project.iam.gserviceaccount.com"
	fc.out["gcloud secrets get-iam-policy shop-DB_URL"] = policyWith(old, "serviceAccount:searchsvc@my-project.iam.gserviceaccount.com",
		"serviceAccount:"+liveSearch, "serviceAccount:websvc@my-project.iam.gserviceaccount.com")
	if res := applySelected(t, s, env); !res.OK {
		t.Fatalf("apply: %+v", res)
	}
	if got, want := revokedFrom(fc), []string{"shop-DB_URL " + old}; !slices.Equal(got, want) {
		t.Errorf("revoked = %q, want %q", got, want)
	}
	grants := linesOf(fc, "gcloud secrets add-iam-policy-binding ")
	if len(grants) != 1 || !strings.Contains(grants[0], "shop-DB_URL --member serviceAccount:websvc@") {
		t.Errorf("grants = %q, want web's account on shop-DB_URL", grants)
	}

	// A dry run names the same, and only that.
	env, fc = selectEnv(t)
	env.DryRun = true
	fc.out["gcloud secrets get-iam-policy shop-DB_URL"] = policyWith(old, "serviceAccount:"+liveSearch)
	res := applySelected(t, s, env)
	var would []string
	for _, m := range res.Messages {
		if strings.HasPrefix(m, "Would take") {
			would = append(would, m)
		}
	}
	if want := []string{"Would take the right to read secret shop-DB_URL away from old@my-project.iam.gserviceaccount.com."}; !res.OK || !slices.Equal(would, want) {
		t.Errorf("dry run: %+v, want %q", res, want)
	}
}

// Criterion 3: a selected internal service keeps letting in the left-out
// services that call it, and a left-out service's own binding isn't changed.
func TestApplySelectedInvokerCallers(t *testing.T) {
	s := selectSpec(t, "")
	env, fc := selectEnv(t)
	if res := applySelected(t, s, env); !res.OK {
		t.Fatalf("apply: %+v", res)
	}
	var members []string
	for _, c := range fc.calls {
		if strings.HasPrefix(c.line(), "gcloud run services add-iam-policy-binding shop-web ") {
			members = append(members, c.args[slices.Index(c.args, "--member")+1])
		}
	}
	slices.Sort(members)
	want := []string{
		"serviceAccount:123456789-compute@developer.gserviceaccount.com", // reels
		"serviceAccount:searchsvc@my-project.iam.gserviceaccount.com",
		"serviceAccount:websvc@my-project.iam.gserviceaccount.com", // web's own cron jobs
	}
	if !slices.Equal(members, want) {
		t.Errorf("run.invoker on shop-web = %q, want %q", members, want)
	}
	if lines := linesOf(fc, "gcloud run services add-iam-policy-binding shop-search"); len(lines) > 0 {
		t.Errorf("a left-out service's binding changed: %q", lines)
	}
}

// Criterion 3: the left-out services' Scheduler jobs are not orphans.
func TestApplySelectedKeepsLeftOutCronJobs(t *testing.T) {
	s := selectSpec(t, "")
	env, fc := selectEnv(t)
	fc.out["gcloud scheduler jobs list"] = "anyship_shop_web_0\nanyship_shop_web_1\nanyship_shop_search_0\nanyship_shop_reels_0\nanyship_shop_gone_0"
	if res := applySelected(t, s, env); !res.OK {
		t.Fatalf("apply: %+v", res)
	}
	var deleted []string
	for _, line := range linesOf(fc, "gcloud scheduler jobs delete ") {
		deleted = append(deleted, strings.Fields(line)[4])
	}
	if want := []string{"anyship_shop_web_1", "anyship_shop_gone_0"}; !slices.Equal(deleted, want) {
		t.Errorf("deleted = %q, want %q", deleted, want)
	}
}

// Criterion 4: a left-out service's URL is the one Cloud Run answers on,
// the same one a full apply passes when Cloud Run answers on that.
func TestApplySelectedResolvesURLFromCloudRun(t *testing.T) {
	s := selectSpec(t, "")
	env, fc := selectEnv(t)
	res := applySelected(t, s, env)
	if deploy := fc.find("gcloud run deploy shop-web"); !res.OK || deploy == nil || !strings.Contains(deploy.line(), "SEARCH="+searchURL+" ") {
		t.Errorf("web wasn't given search's URL: %+v %+v", res, deploy)
	}
	if got := codesOf(res.Findings, adapter.Warning); slices.Contains(got, "GCP_SERVICE_URL_LEGACY") {
		t.Errorf("warnings = %q", got)
	}

	// A service that only answers on another URL is called there, with a
	// warning.
	env, fc = selectEnv(t)
	fc.out["gcloud run services list"] = runningSearch(`["` + searchLegacyURL + `"]`)
	res = applySelected(t, s, env)
	if deploy := fc.find("gcloud run deploy shop-web"); !res.OK || deploy == nil || !strings.Contains(deploy.line(), "SEARCH="+searchLegacyURL+" ") {
		t.Errorf("web wasn't given search's URL: %+v %+v", res, deploy)
	}
	if got := codesOf(res.Findings, adapter.Warning); !slices.Equal(got, []string{"GCP_SERVICE_URL_LEGACY"}) {
		t.Errorf("warnings = %q, want GCP_SERVICE_URL_LEGACY", got)
	}
	// A warning doesn't fail the checks, so they still say they passed.
	if !slices.Contains(codesOf(res.Findings, adapter.Info), "GCP_PREFLIGHT_OK") {
		t.Errorf("findings = %+v, want GCP_PREFLIGHT_OK", res.Findings)
	}
}

// Cloud Run may leave out the account a service runs as, and its URL: the
// default compute account keeps its access, and the URL is the formula one.
func TestApplySelectedLiveFallbacks(t *testing.T) {
	s := selectSpec(t, "")
	env, fc := selectEnv(t)
	// reels is selected, so only Cloud Run says search runs as the default
	// account.
	env.Services = []string{"web", "reels"}
	fc.out["gcloud run services list"] = `[{"metadata": {"name": "shop-search"}, "spec": {"template": {"spec": {}}}, "status": {"url": ""}}]`
	const def = "123456789-compute@developer.gserviceaccount.com"
	const old = "old@my-project.iam.gserviceaccount.com"
	fc.out["gcloud secrets get-iam-policy shop-DB_URL"] = policyWith("serviceAccount:"+def, "serviceAccount:"+old)
	res := applySelected(t, s, env)
	if !res.OK {
		t.Fatalf("apply: %+v", res)
	}
	if got, want := revokedFrom(fc), []string{"shop-DB_URL serviceAccount:" + old}; !slices.Equal(got, want) {
		t.Errorf("revoked = %q, want %q", got, want)
	}
	if deploy := fc.find("gcloud run deploy shop-web"); deploy == nil || !strings.Contains(deploy.line(), "SEARCH="+searchURL+" ") {
		t.Errorf("web wasn't given search's formula URL: %+v", deploy)
	}
	if got := codesOf(res.Findings, adapter.Warning); slices.Contains(got, "GCP_SERVICE_URL_LEGACY") {
		t.Errorf("warnings = %q", got)
	}
}

// A secret written from the environment that a left-out service lists too
// is called out; one the run doesn't write isn't.
func TestPlanSelectedWarnsSharedSecret(t *testing.T) {
	s := selectSpec(t, "")
	env, _ := selectEnv(t)
	if got := codes(plan(t, s, env), adapter.Warning); slices.Contains(got, "GCP_SECRET_SHARED_LEFT_OUT") {
		t.Errorf("warned without $DB_URL: %q", got)
	}

	env.LookupEnv = func(key string) (string, bool) { return "postgres://new", key == "DB_URL" || key == "SEARCH_KEY" }
	p := plan(t, s, env)
	var shared []adapter.Finding
	for _, f := range p.Findings {
		if f.Code == "GCP_SECRET_SHARED_LEFT_OUT" {
			shared = append(shared, f)
		}
	}
	if len(shared) != 1 || shared[0].Level != adapter.Warning || !strings.Contains(shared[0].Message, "shop-DB_URL") ||
		!strings.Contains(shared[0].Message, "search list(s) it too") || !strings.Contains(shared[0].Hint, "--service search") {
		t.Errorf("findings = %+v, want one warning for shop-DB_URL and search", shared)
	}

	// Nothing is left out that lists it.
	env.Services = []string{"web", "search"}
	if got := codes(plan(t, s, env), adapter.Warning); slices.Contains(got, "GCP_SECRET_SHARED_LEFT_OUT") {
		t.Errorf("warned with search selected: %q", got)
	}

	// Two left-out services list it: the hint adds both.
	s = selectSpec(t, `, "secrets": ["DB_URL"]`)
	env.Services = []string{"web"}
	p = plan(t, s, env)
	i := slices.IndexFunc(p.Findings, func(f adapter.Finding) bool { return f.Code == "GCP_SECRET_SHARED_LEFT_OUT" })
	if want := "Run `anyship apply` without --service, or add --service reels --service search."; i < 0 || p.Findings[i].Hint != want {
		t.Errorf("findings = %+v, want the hint %q", p.Findings, want)
	}
}

// Criterion 4: a run fails before any change when a service it deploys
// refers to a left-out service that isn't running.
func TestApplySelectedFailsWhenReferenceNotDeployed(t *testing.T) {
	s := selectSpec(t, "")
	env, fc := selectEnv(t)
	fc.out["gcloud run services list"] = "[]"
	res := applySelected(t, s, env)
	if res.OK || !slices.Equal(codesOf(res.Findings, adapter.Error), []string{"GCP_SERVICE_NOT_DEPLOYED"}) {
		t.Fatalf("apply: %+v", res)
	}
	if f := res.Findings[slices.IndexFunc(res.Findings, func(f adapter.Finding) bool { return f.Code == "GCP_SERVICE_NOT_DEPLOYED" })]; !strings.Contains(f.Message, "shop-search isn't deployed in my-project/us-central1") || !strings.Contains(f.Hint, "--service search") {
		t.Errorf("finding = %+v", f)
	}
	// Nothing says the checks passed next to the one that failed.
	if slices.Contains(codesOf(res.Findings, adapter.Info), "GCP_PREFLIGHT_OK") {
		t.Errorf("findings = %+v, want no GCP_PREFLIGHT_OK", res.Findings)
	}
	for _, prefix := range []string{"gcloud secrets", "gcloud run deploy", "gcloud scheduler", "gcloud run services add-iam-policy-binding"} {
		if c := fc.find(prefix); c != nil {
			t.Errorf("changed something before failing: %s", c.line())
		}
	}
}

// A run that can't read the left-out services fails before any change, and
// doesn't say the checks passed either.
func TestApplySelectedFailsWhenLiveServicesUnreadable(t *testing.T) {
	s := selectSpec(t, "")
	env, fc := selectEnv(t)
	fc.fail = []string{"gcloud run services list"}
	res := applySelected(t, s, env)
	if res.OK || len(res.Messages) != 1 || !strings.Contains(res.Messages[0], "listing Cloud Run services in my-project/us-central1 failed") {
		t.Fatalf("apply: %+v", res)
	}
	if slices.Contains(codesOf(res.Findings, adapter.Info), "GCP_PREFLIGHT_OK") {
		t.Errorf("findings = %+v, want no GCP_PREFLIGHT_OK", res.Findings)
	}
	for _, prefix := range []string{"gcloud secrets", "gcloud run deploy", "gcloud scheduler", "gcloud run services add-iam-policy-binding"} {
		if c := fc.find(prefix); c != nil {
			t.Errorf("changed something before failing: %s", c.line())
		}
	}
}

// A left-out service whose URL label is too long is refused like in a full
// apply, not called on the URL Cloud Run reports, when a selected service
// refers to it.
func TestApplySelectedRefusesLongURLLabel(t *testing.T) {
	long := "s" + strings.Repeat("a", 43)
	s := parse(t, `{"name": "shop",
		"services": {
			"web": {"kind": "server", "image": "web:1", "env": {"SEARCH": "${services.`+long+`.url}"}},
			"`+long+`": {"kind": "server", "image": "search:1"},
			"reels": {"kind": "server", "image": "reels:1"}},
		`+target+`}`)
	run := func(t *testing.T, services ...string) (*adapter.Result, *fakeCloud) {
		t.Helper()
		env, fc := newEnv(t, t.TempDir(), nil)
		env.Services = services
		fc.out["gcloud projects describe my-project"] = longNumber
		fc.out["gcloud run services list"] = `[{"metadata": {"name": "shop-` + long + `"}, "spec": {"template": {"spec": {}}}, "status": {"url": "` + searchLegacyURL + `"}}]`
		return applySelected(t, s, env), fc
	}
	for _, services := range [][]string{nil, {"web"}} {
		res, fc := run(t, services...)
		if res.OK || !slices.Equal(codesOf(res.Findings, adapter.Error), []string{"GCP_NAME_URL"}) || slices.Contains(codesOf(res.Findings, adapter.Warning), "GCP_SERVICE_URL_LEGACY") {
			t.Errorf("services %q: %+v", services, res)
		} else if f := res.Findings[slices.IndexFunc(res.Findings, func(f adapter.Finding) bool { return f.Code == "GCP_NAME_URL" })]; f.Service != long {
			t.Errorf("services %q: finding = %+v", services, f)
		}
		if c := fc.find("gcloud run deploy"); c != nil {
			t.Errorf("services %q: deployed %s", services, c.line())
		}
	}

	// A run that doesn't refer to it leaves it to the run that deploys it.
	if res, fc := run(t, "reels"); !res.OK || fc.find("gcloud run deploy shop-reels") == nil {
		t.Errorf("reels: %+v", res)
	}
}

// GCP_NAME_URL doesn't stop preflight: the run reports the other problems
// with it. A left-out service that two selected ones refer to is refused once.
func TestApplyLongURLLabelWithOtherFindings(t *testing.T) {
	long := "s" + strings.Repeat("a", 43)
	s := parse(t, `{"name": "shop",
		"services": {
			"web": {"kind": "server", "image": "web:1", "env": {"SEARCH": "${services.`+long+`.url}"}},
			"api": {"kind": "server", "image": "api:1", "env": {"SEARCH": "${services.`+long+`.url}"}},
			"`+long+`": {"kind": "server", "image": "search:1"}},
		`+target+`}`)
	for _, services := range [][]string{nil, {"web"}, {"web", "api"}} {
		env, fc := newEnv(t, t.TempDir(), nil)
		env.Services = services
		fc.out["gcloud projects describe my-project"] = longNumber
		fc.out["gcloud services list"] = "compute.googleapis.com"
		fc.out["gcloud run services list"] = `[{"metadata": {"name": "shop-` + long + `"}, "spec": {"template": {"spec": {}}}, "status": {"url": "` + searchLegacyURL + `"}}]`
		res := applySelected(t, s, env)
		if res.OK || !slices.Equal(codesOf(res.Findings, adapter.Error), []string{"GCP_NAME_URL", "GCP_PREFLIGHT_API"}) || slices.Contains(codesOf(res.Findings, adapter.Info), "GCP_PREFLIGHT_OK") {
			t.Errorf("services %q: %+v", services, res)
		}
		if c := fc.find("gcloud run deploy"); c != nil {
			t.Errorf("services %q: deployed %s", services, c.line())
		}
	}
}

// A filtered plan runs nothing, shows a placeholder for a left-out service's
// URL, and renders the same files every time.
func TestPlanSelectedHasNoSideEffects(t *testing.T) {
	s := selectSpec(t, "")
	env, fc := selectEnv(t)
	p := plan(t, s, env)
	if len(fc.calls) > 0 {
		t.Errorf("plan ran %q", fc.calls[0].line())
	}
	if len(p.Files) != 1 || filepath.Base(p.Files[0].Path) != "web.gcloud.txt" || !strings.Contains(string(p.Files[0].Contents), "SEARCH=<URL of shop-search as deployed>") {
		t.Errorf("files = %+v", p.Files)
	}
	again := plan(t, s, env)
	if len(again.Files) != 1 || string(again.Files[0].Contents) != string(p.Files[0].Contents) {
		t.Error("two plans rendered different files")
	}
	if _, err := os.Stat(env.OutDir); !os.IsNotExist(err) {
		t.Errorf("plan wrote %s: %v", env.OutDir, err)
	}
}

// Once the checks have read it, a dry run writes the URL a left-out service
// answers on into the deploy command, as the real apply passes it.
func TestDryRunSelectedWritesResolvedURL(t *testing.T) {
	s := selectSpec(t, "")
	env, _ := selectEnv(t)
	env.DryRun = true
	if res := applySelected(t, s, env); !res.OK {
		t.Fatalf("dry run: %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(env.OutDir, "web.gcloud.txt"))
	if err != nil || !strings.Contains(string(got), "SEARCH="+searchURL+" ") || strings.Contains(string(got), "<URL of") {
		t.Errorf("web.gcloud.txt = %s, %v; want SEARCH=%s", got, err, searchURL)
	}

	// The project number is read by then too, in a full run as well.
	env.Services = nil
	if res := applySelected(t, s, env); !res.OK {
		t.Fatalf("full dry run: %+v", res)
	}
	got, err = os.ReadFile(filepath.Join(env.OutDir, "reels.gcloud.txt"))
	if err != nil || !strings.Contains(string(got), "--service-account 123456789-compute@developer.gserviceaccount.com ") || strings.Contains(string(got), numberPlaceholder) {
		t.Errorf("reels.gcloud.txt = %s, %v; want the default account with the project number", got, err)
	}
}

// Criterion 6: the plan says what it leaves untouched, and what the left-out
// services keep. Their own findings don't block the run.
func TestPlanSelectedSaysWhatIsUntouched(t *testing.T) {
	s := selectSpec(t, `, "volumes": [{"name": "data", "mountPath": "/data", "size": "1GB"}]`)
	env, _ := selectEnv(t)
	p := plan(t, s, env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Fatalf("a left-out service's finding blocks the plan: %v", errs)
	}
	untouched := map[string]string{}
	for _, a := range p.Actions {
		if a.Op == adapter.OpNote && a.Kind == "Cloud Run service" {
			untouched[a.Name] = a.Detail
		}
	}
	want := map[string]string{
		"shop-search": "left untouched (not selected with --service); its Cloud Scheduler jobs are kept; secret(s) no selected service lists are kept: shop-SEARCH_KEY",
		"shop-reels":  "left untouched (not selected with --service); its Cloud Scheduler jobs are kept",
	}
	if len(untouched) != len(want) || untouched["shop-search"] != want["shop-search"] || untouched["shop-reels"] != want["shop-reels"] {
		t.Errorf("untouched = %q, want %q", untouched, want)
	}
	// web calls search, which is internal: its invoker binding isn't looked at.
	i := slices.IndexFunc(p.Findings, func(f adapter.Finding) bool { return f.Code == "GCP_SERVICE_LEFT_OUT_INTERNAL" })
	if i < 0 || p.Findings[i].Level != adapter.Warning || p.Findings[i].Service != "web" || !strings.Contains(p.Findings[i].Hint, "--service search") {
		t.Errorf("findings = %+v", p.Findings)
	}
	for _, f := range p.Findings {
		if f.Service == "search" || f.Service == "reels" {
			t.Errorf("a left-out service's finding is in the plan: %+v", f)
		}
	}

	// Without a selection the same spec is refused for reels' volume.
	env.Services = nil
	if errs := codes(plan(t, s, env), adapter.Error); !slices.Equal(errs, []string{"GCP_VOLUMES"}) {
		t.Errorf("full plan errors = %v", errs)
	}
}

// Criterion 7: without a selection an apply runs the same commands as before
// --service existed, recorded from 3aa121b.
func TestApplyWithoutSelectionUnchanged(t *testing.T) {
	s := selectSpec(t, "")
	env, fc := newEnv(t, t.TempDir(), nil)
	schedulerEnabled(fc)
	fc.out["gcloud run services list"] = runningSearch(`["` + searchURL + `"]`)
	fc.out["gcloud secrets describe shop-DB_URL"] = "shop"
	fc.out["gcloud secrets describe shop-SEARCH_KEY"] = "shop"
	fc.out["gcloud secrets get-iam-policy shop-DB_URL"] = policyWith("serviceAccount:old@my-project.iam.gserviceaccount.com", "serviceAccount:searchsvc@my-project.iam.gserviceaccount.com")
	fc.out["gcloud scheduler jobs list"] = "anyship_shop_web_0\nanyship_shop_search_0\nanyship_shop_reels_0\nanyship_shop_gone_0"
	if res := applySelected(t, s, env); !res.OK {
		t.Fatalf("apply: %+v", res)
	}
	want := []string{
		"gcloud config get-value account --project my-project --quiet",
		"gcloud auth print-access-token --project my-project --quiet",
		"gcloud --version",
		"gcloud projects describe my-project --format value(projectNumber) --project my-project --quiet",
		"gcloud services list --enabled --format value(config.name) --project my-project --quiet",
		"gcloud secrets describe shop-DB_URL --project my-project --quiet",
		"gcloud secrets describe shop-SEARCH_KEY --project my-project --quiet",
		"gcloud secrets add-iam-policy-binding shop-DB_URL --member serviceAccount:searchsvc@my-project.iam.gserviceaccount.com --role roles/secretmanager.secretAccessor --project my-project --quiet",
		"gcloud secrets add-iam-policy-binding shop-SEARCH_KEY --member serviceAccount:searchsvc@my-project.iam.gserviceaccount.com --role roles/secretmanager.secretAccessor --project my-project --quiet",
		"gcloud secrets add-iam-policy-binding shop-DB_URL --member serviceAccount:websvc@my-project.iam.gserviceaccount.com --role roles/secretmanager.secretAccessor --project my-project --quiet",
		"gcloud secrets describe shop-DB_URL --format value(labels.anyship-project) --project my-project --quiet",
		"gcloud secrets get-iam-policy shop-DB_URL --format json --project my-project --quiet",
		"gcloud secrets describe shop-SEARCH_KEY --format value(labels.anyship-project) --project my-project --quiet",
		"gcloud secrets get-iam-policy shop-SEARCH_KEY --format json --project my-project --quiet",
		"gcloud secrets remove-iam-policy-binding shop-DB_URL --member serviceAccount:old@my-project.iam.gserviceaccount.com --role roles/secretmanager.secretAccessor --project my-project --quiet",
		"gcloud scheduler jobs list --location us-central1 --format value(name.basename()) --project my-project --quiet",
		"gcloud run deploy shop-reels --image reels:1 --region us-central1 --project my-project --port 80 --labels anyship-project=shop,anyship-service=reels --memory 512Mi --cpu 1 --ingress all --allow-unauthenticated --clear-env-vars --clear-secrets --min-instances default --max-instances default --concurrency default --timeout 300 --service-account 123456789-compute@developer.gserviceaccount.com --startup-probe  --quiet",
		"gcloud run services describe shop-reels --region us-central1 --format value(status.url) --project my-project --quiet",
		"gcloud scheduler jobs describe anyship_shop_reels_0 --location us-central1 --project my-project --quiet",
		"gcloud scheduler jobs update http anyship_shop_reels_0 --location us-central1 --schedule 0 3 * * * --time-zone Etc/UTC --uri https://shop-reels-123456789.us-central1.run.app/sweep --http-method post --attempt-deadline 300s --oidc-service-account-email 123456789-compute@developer.gserviceaccount.com --oidc-token-audience https://shop-reels-123456789.us-central1.run.app --description anyship: shop/reels cron \"0 3 * * *\" --clear-headers --clear-message-body --project my-project --quiet",
		"gcloud run deploy shop-search --image search:1 --region us-central1 --project my-project --port 8080 --labels anyship-project=shop,anyship-service=search --memory 512Mi --cpu 1 --ingress all --no-allow-unauthenticated --clear-env-vars --set-secrets DB_URL=shop-DB_URL:latest,SEARCH_KEY=shop-SEARCH_KEY:latest --min-instances default --max-instances default --concurrency default --timeout 300 --service-account searchsvc@my-project.iam.gserviceaccount.com --startup-probe  --quiet",
		"gcloud run services describe shop-search --region us-central1 --format value(status.url) --project my-project --quiet",
		"gcloud run services add-iam-policy-binding shop-search --region us-central1 --member serviceAccount:123456789-compute@developer.gserviceaccount.com --role roles/run.invoker --project my-project --quiet",
		"gcloud run services add-iam-policy-binding shop-search --region us-central1 --member serviceAccount:websvc@my-project.iam.gserviceaccount.com --role roles/run.invoker --project my-project --quiet",
		"gcloud run services add-iam-policy-binding shop-search --region us-central1 --member serviceAccount:searchsvc@my-project.iam.gserviceaccount.com --role roles/run.invoker --project my-project --quiet",
		"gcloud scheduler jobs describe anyship_shop_search_0 --location us-central1 --project my-project --quiet",
		"gcloud scheduler jobs update http anyship_shop_search_0 --location us-central1 --schedule 0 * * * * --time-zone Etc/UTC --uri https://shop-search-123456789.us-central1.run.app/tick --http-method post --attempt-deadline 300s --oidc-service-account-email searchsvc@my-project.iam.gserviceaccount.com --oidc-token-audience https://shop-search-123456789.us-central1.run.app --description anyship: shop/search cron \"0 * * * *\" --clear-headers --clear-message-body --project my-project --quiet",
		"gcloud run deploy shop-web --image web:1 --region us-central1 --project my-project --port 80 --labels anyship-project=shop,anyship-service=web --memory 512Mi --cpu 1 --ingress all --no-allow-unauthenticated --set-env-vars SEARCH=https://shop-search-123456789.us-central1.run.app --set-secrets DB_URL=shop-DB_URL:latest --min-instances default --max-instances default --concurrency default --timeout 300 --service-account websvc@my-project.iam.gserviceaccount.com --startup-probe  --quiet",
		"gcloud run services describe shop-web --region us-central1 --format value(status.url) --project my-project --quiet",
		"gcloud run services add-iam-policy-binding shop-web --region us-central1 --member serviceAccount:123456789-compute@developer.gserviceaccount.com --role roles/run.invoker --project my-project --quiet",
		"gcloud run services add-iam-policy-binding shop-web --region us-central1 --member serviceAccount:searchsvc@my-project.iam.gserviceaccount.com --role roles/run.invoker --project my-project --quiet",
		"gcloud run services add-iam-policy-binding shop-web --region us-central1 --member serviceAccount:websvc@my-project.iam.gserviceaccount.com --role roles/run.invoker --project my-project --quiet",
		"gcloud scheduler jobs describe anyship_shop_web_0 --location us-central1 --project my-project --quiet",
		"gcloud scheduler jobs update http anyship_shop_web_0 --location us-central1 --schedule * * * * * --time-zone Etc/UTC --uri https://shop-web-123456789.us-central1.run.app/tick --http-method post --attempt-deadline 300s --oidc-service-account-email websvc@my-project.iam.gserviceaccount.com --oidc-token-audience https://shop-web-123456789.us-central1.run.app --description anyship: shop/web cron \"* * * * *\" --clear-headers --clear-message-body --project my-project --quiet",
		"gcloud scheduler jobs delete anyship_shop_gone_0 --location us-central1 --project my-project --quiet",
	}
	var got []string
	for _, c := range fc.calls {
		got = append(got, c.line())
	}
	if !slices.Equal(got, want) {
		t.Errorf("calls changed:\n got  %q\n want %q", got, want)
	}
}
