package gcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/image"
	"github.com/j75689/anyship/spec"
)

// gcloud runs gcloud commands for one project, in the configured gcloud
// configuration.
type gcloud struct {
	env           *adapter.Env
	project       string
	configuration string
}

func newGcloud(env *adapter.Env, o Options) gcloud {
	return gcloud{env: env, project: o.Project, configuration: o.Configuration}
}

func (g gcloud) options(in io.Reader, out, errOut io.Writer) adapter.ExecOptions {
	opts := adapter.ExecOptions{Dir: g.env.Dir, Stdin: in, Stdout: out, Stderr: errOut}
	if g.configuration != "" {
		opts.Env = []string{"CLOUDSDK_ACTIVE_CONFIG_NAME=" + g.configuration}
	}
	return opts
}

// run executes gcloud with --project and --quiet; stdout is captured when out
// is set and stdin is fed from in.
func (g gcloud) run(ctx context.Context, in io.Reader, out io.Writer, args ...string) error {
	args = append(args, "--project", g.project, "--quiet")
	return g.env.Exec(ctx, g.options(in, out, nil), "gcloud", args...)
}

func (g gcloud) output(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	err := g.run(ctx, nil, &out, args...)
	return strings.TrimSpace(out.String()), err
}

// probe runs a check whose failure anyship reports itself. gcloud's error
// output is kept out of the terminal and folded into the error instead.
func (g gcloud) probe(ctx context.Context, args ...string) (string, error) {
	var out, errOut bytes.Buffer
	args = append(args, "--project", g.project, "--quiet")
	if err := g.env.Exec(ctx, g.options(nil, &out, &errOut), "gcloud", args...); err != nil {
		if msg := gcloudError(errOut.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// gcloudError picks the ERROR: line out of gcloud's error output.
func gcloudError(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for _, line := range lines {
		if msg, ok := strings.CutPrefix(line, "ERROR: "); ok {
			return msg
		}
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// Apply runs the preflight checks, then sets secrets, builds and pushes
// images, and deploys every service. With env.DryRun it stops after the checks.
func (a *Adapter) Apply(ctx context.Context, plan *adapter.Plan, _ *spec.Spec, env *adapter.Env) (*adapter.Result, error) {
	if adapter.HasErrors(plan.Findings) {
		return &adapter.Result{Messages: []string{"The plan has errors; fix them and plan again."}}, nil
	}
	data, ok := plan.Data.(*planData)
	if !ok {
		return nil, fmt.Errorf("plan was not produced by the %s adapter", Name)
	}
	for _, f := range plan.Files {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(f.Path, f.Contents, 0o644); err != nil {
			return nil, err
		}
	}

	g := newGcloud(env, data.opts)
	env.Logf("checking project %s in %s", data.opts.Project, data.opts.Region)
	defaultAccount, checks := preflight(ctx, g, data)
	result := func(ok bool, messages ...string) *adapter.Result {
		return &adapter.Result{OK: ok, Findings: checks, Messages: messages}
	}
	if adapter.HasErrors(checks) {
		return result(false, "Preflight checks failed; nothing was changed."), nil
	}
	for i := range data.services {
		if data.services[i].account == "" {
			data.services[i].account = defaultAccount
		}
	}
	if env.DryRun {
		// Say which secret access the apply would take away, since that
		// is the one thing it does that isn't in the plan.
		stale, err := staleReaders(ctx, g, data)
		if err != nil {
			return result(false, err.Error()), nil
		}
		messages := []string{fmt.Sprintf("Dry run: preflight checks passed for %s; nothing was changed.", data.opts.Project)}
		for _, r := range stale {
			messages = append(messages, fmt.Sprintf("Would take the right to read secret %s away from %s.", r.secret, r.account))
		}
		return result(true, messages...), nil
	}

	for _, sc := range data.secrets {
		if err := ensureSecret(ctx, g, data.project, sc); err != nil {
			return result(false, err.Error()), nil
		}
	}
	// Each account may read the secrets its own services list, and no others.
	granted := map[[2]string]bool{}
	for _, sv := range data.services {
		for _, name := range sv.svc.Secrets {
			grant := [2]string{secretID(data.project, name), sv.account}
			if granted[grant] {
				continue
			}
			granted[grant] = true
			if err := grantAccess(ctx, g, grant[0], grant[1]); err != nil {
				return result(false, err.Error()), nil
			}
		}
	}
	stale, err := staleReaders(ctx, g, data)
	if err != nil {
		return result(false, err.Error()), nil
	}
	for _, r := range stale {
		if err := revokeAccess(ctx, g, r.secret, r.account); err != nil {
			return result(false, err.Error()), nil
		}
	}

	if slices.ContainsFunc(data.services, func(sv service) bool { return sv.build != nil }) {
		if err := dockerLogin(ctx, g, data.opts); err != nil {
			return result(false, err.Error()), nil
		}
	}
	for i := range data.services {
		sv := &data.services[i]
		if sv.build == nil {
			continue
		}
		path := sv.build.dockerfile
		if sv.build.generated != nil {
			var err error
			if path, err = image.WriteGenerated(env.OutDir, sv.name, sv.build.generated); err != nil {
				return nil, err
			}
		}
		ref, err := image.BuildAndPush(ctx, env, image.Build{Context: sv.build.contextDir, Dockerfile: path, Repository: data.opts.imageRepository(sv.cloudRun)})
		if err != nil {
			return result(false, err.Error()), nil
		}
		sv.image = ref
	}

	// The spec's jobs as they are now, to clear away those no entry asks
	// for any more. Without the API there can be none.
	var jobs []string
	if data.scheduler {
		var err error
		if jobs, err = listJobs(ctx, g, data.opts, data.project); err != nil {
			return result(false, err.Error()), nil
		}
	}

	messages := []string{fmt.Sprintf("Deployed %s to Cloud Run in %s/%s.", data.project, data.opts.Project, data.opts.Region)}
	for _, sv := range data.services {
		env.Logf("$ gcloud run deploy %s", sv.cloudRun)
		if err := g.env.Exec(ctx, g.options(nil, nil, nil), "gcloud", deployArgs(data, sv, sv.image)...); err != nil {
			return result(false, fmt.Sprintf("gcloud run deploy %s failed: %v", sv.cloudRun, err)), nil
		}
		url, err := g.output(ctx, "run", "services", "describe", sv.cloudRun, "--region", data.opts.Region, "--format", "value(status.url)")
		if err == nil && url != "" {
			messages = append(messages, fmt.Sprintf("%s: %s", sv.name, url))
		}
		if sv.internal {
			if err := allowCallers(ctx, g, data, sv); err != nil {
				return result(false, err.Error()), nil
			}
		}
		if len(sv.jobs) > 0 {
			if url == "" {
				return result(false, fmt.Sprintf("%s is deployed, but its URL couldn't be read to schedule its cron entries (%v).", sv.cloudRun, err)), nil
			}
			if err := applyJobs(ctx, g, data, sv, url); err != nil {
				return result(false, err.Error()), nil
			}
			messages = append(messages, fmt.Sprintf("%s: %d cron schedule(s) on Cloud Scheduler", sv.name, len(sv.jobs)))
		}
		// Right away, not after every service: an entry taken out of the
		// spec must stop firing even if a later deploy fails.
		if _, err := deleteJobs(ctx, g, data.opts, jobsOf(jobs, data.project, sv.name), sv.wantsJob); err != nil {
			return result(false, err.Error()), nil
		}
	}
	// What is left belongs to services the spec no longer has.
	if _, err := deleteJobs(ctx, g, data.opts, orphanJobs(jobs, data), nil); err != nil {
		return result(false, err.Error()), nil
	}
	return result(true, messages...), nil
}

var requiredAPIs = map[string]string{
	"run":      "run.googleapis.com",
	"registry": "artifactregistry.googleapis.com",
	"secrets":  "secretmanager.googleapis.com",
	"cron":     "cloudscheduler.googleapis.com",
}

// preflight checks the login, project, APIs and registry before anything
// changes, and returns the project's Compute Engine default service account,
// which a service runs as when the spec names no other.
func preflight(ctx context.Context, g gcloud, d *planData) (string, []adapter.Finding) {
	var findings []adapter.Finding
	add := func(level adapter.Level, code, message, hint string) {
		findings = append(findings, adapter.Finding{Level: level, Code: code, Message: message, Hint: hint})
	}

	account, _ := g.probe(ctx, "config", "get-value", "account")
	if _, err := g.probe(ctx, "auth", "print-access-token"); err != nil || account == "" {
		hint := "Run `gcloud auth login`."
		if g.configuration != "" {
			hint = fmt.Sprintf("Run `gcloud auth login`, then `gcloud config set account <email> --configuration %s`.", g.configuration)
		}
		add(adapter.Error, "GCP_PREFLIGHT_AUTH", "gcloud isn't logged in.", hint)
		return "", findings
	}
	number, err := g.probe(ctx, "projects", "describe", d.opts.Project, "--format", "value(projectNumber)")
	if err != nil || number == "" {
		add(adapter.Error, "GCP_PREFLIGHT_PROJECT", fmt.Sprintf("%s can't read project %s; check that it exists and that this account has access (%v).", account, d.opts.Project, err),
			"Use another account (`gcloud config set account`, or a configuration via spec.targets.gcp.configuration), or grant this one access to the project.")
		return "", findings
	}
	d.number = number
	runAs := number + defaultAccountSuffix

	enabled, err := g.probe(ctx, "services", "list", "--enabled", "--format", "value(config.name)")
	if err != nil {
		add(adapter.Error, "GCP_PREFLIGHT_PROJECT", fmt.Sprintf("%s can't list the enabled APIs of %s.", account, d.opts.Project), "")
		return runAs, findings
	}
	building := slices.ContainsFunc(d.services, func(sv service) bool { return sv.build != nil })
	needed := []string{requiredAPIs["run"]}
	if building {
		needed = append(needed, requiredAPIs["registry"])
	}
	if len(d.secrets) > 0 {
		needed = append(needed, requiredAPIs["secrets"])
	}
	if slices.ContainsFunc(d.services, func(sv service) bool { return len(sv.jobs) > 0 }) {
		needed = append(needed, requiredAPIs["cron"])
	}
	apis := strings.Fields(enabled)
	d.scheduler = slices.Contains(apis, requiredAPIs["cron"])
	for _, api := range needed {
		if !slices.Contains(apis, api) {
			add(adapter.Error, "GCP_PREFLIGHT_API", fmt.Sprintf("The %s API isn't enabled in %s.", api, d.opts.Project),
				fmt.Sprintf("gcloud services enable %s --project %s", api, d.opts.Project))
		}
	}

	if building && slices.Contains(apis, requiredAPIs["registry"]) {
		format, err := g.probe(ctx, "artifacts", "repositories", "describe", d.opts.Repository, "--location", d.opts.Region, "--format", "value(format)")
		switch {
		case err != nil && !strings.Contains(err.Error(), "NOT_FOUND"):
			add(adapter.Error, "GCP_PREFLIGHT_REPOSITORY", fmt.Sprintf("Couldn't check Artifact Registry repository %s in %s: %v.", d.opts.Repository, d.opts.Region, err), "")
		case err != nil:
			add(adapter.Error, "GCP_PREFLIGHT_REPOSITORY", fmt.Sprintf("Artifact Registry repository %s doesn't exist in %s.", d.opts.Repository, d.opts.Region),
				fmt.Sprintf("gcloud artifacts repositories create %s --repository-format docker --location %s --project %s", d.opts.Repository, d.opts.Region, d.opts.Project))
		case format != "DOCKER":
			add(adapter.Error, "GCP_PREFLIGHT_REPOSITORY", fmt.Sprintf("Artifact Registry repository %s holds %s packages, not Docker images.", d.opts.Repository, format), "")
		}
	}
	if building {
		if err := g.env.Exec(ctx, adapter.ExecOptions{Dir: g.env.Dir, Stdout: io.Discard, Stderr: io.Discard}, "docker", "buildx", "version"); err != nil {
			add(adapter.Error, "GCP_PREFLIGHT_DOCKER", "Building from source needs Docker with buildx on this machine.", "Install Docker Desktop or the buildx plugin, or set services.<name>.image.")
		}
	}
	if !adapter.HasErrors(findings) {
		add(adapter.Info, "GCP_PREFLIGHT_OK", fmt.Sprintf("Project %s is ready for %s: the required APIs are enabled.", d.opts.Project, account), "")
	}
	return runAs, findings
}

// ensureSecret makes the secret exist with the right value: a value from the
// deployer's environment always wins, a missing generated secret gets a random
// value, and an existing secret is otherwise left alone.
func ensureSecret(ctx context.Context, g gcloud, project string, sc secret) error {
	value, fromEnv := g.env.LookupEnv(sc.name)
	fromEnv = fromEnv && value != ""
	_, err := g.probe(ctx, "secrets", "describe", sc.id)
	exists := err == nil

	switch {
	case exists && !fromEnv:
		return nil
	case exists:
		g.env.Logf("$ gcloud secrets versions add %s (value from $%s)", sc.id, sc.name)
		if err := g.run(ctx, strings.NewReader(value), io.Discard, "secrets", "versions", "add", sc.id, "--data-file", "-"); err != nil {
			return fmt.Errorf("updating secret %s failed: %w", sc.id, err)
		}
		return nil
	case !fromEnv && !sc.generate:
		return fmt.Errorf("secret %s has no value: export %s and apply again", sc.id, sc.name)
	case !fromEnv:
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		value = hex.EncodeToString(buf)
	}
	g.env.Logf("$ gcloud secrets create %s", sc.id)
	err = g.run(ctx, strings.NewReader(value), io.Discard, "secrets", "create", sc.id,
		"--replication-policy", "automatic", "--data-file", "-", "--labels", projectLabel+"="+project)
	if err != nil {
		return fmt.Errorf("creating secret %s failed: %w", sc.id, err)
	}
	return nil
}

// allowCallers lets the accounts the spec's other services run as call an
// internal service. The binding is idempotent, so every apply can make sure
// it exists.
func allowCallers(ctx context.Context, g gcloud, d *planData, sv service) error {
	var accounts []string
	for _, other := range d.services {
		if other.name != sv.name && !slices.Contains(accounts, other.account) {
			accounts = append(accounts, other.account)
		}
	}
	for _, account := range accounts {
		g.env.Logf("$ gcloud run services add-iam-policy-binding %s (run.invoker for %s)", sv.cloudRun, account)
		err := g.run(ctx, nil, io.Discard, "run", "services", "add-iam-policy-binding", sv.cloudRun, "--region", d.opts.Region,
			"--member", "serviceAccount:"+account, "--role", "roles/run.invoker")
		if err != nil {
			return fmt.Errorf("letting %s call %s failed: %w", account, sv.cloudRun, err)
		}
	}
	return nil
}

// reader is a service account's right to read a secret.
type reader struct{ secret, account string }

// staleReaders finds the service accounts that may read the spec's secrets
// without the spec saying so: not the account of a service that lists the
// secret, and not in targets.gcp.secretReaders. Only secrets anyship made
// for the spec are looked at (they carry its label), and only service
// accounts: users, groups and other roles are left alone.
func staleReaders(ctx context.Context, g gcloud, d *planData) ([]reader, error) {
	var stale []reader
	for _, sc := range d.secrets {
		label, err := g.probe(ctx, "secrets", "describe", sc.id, "--format", "value(labels."+projectLabel+")")
		if err != nil || label != d.project {
			continue
		}
		out, err := g.probe(ctx, "secrets", "get-iam-policy", sc.id, "--format", "json")
		if err != nil {
			return nil, fmt.Errorf("reading who may read secret %s failed (%w)", sc.id, err)
		}
		var policy struct {
			Bindings []struct {
				Role    string   `json:"role"`
				Members []string `json:"members"`
			} `json:"bindings"`
		}
		if out != "" {
			if err := json.Unmarshal([]byte(out), &policy); err != nil {
				return nil, fmt.Errorf("unexpected output from gcloud secrets get-iam-policy %s: %w", sc.id, err)
			}
		}
		for _, b := range policy.Bindings {
			if b.Role != secretAccessorRole {
				continue
			}
			for _, member := range b.Members {
				account, ok := strings.CutPrefix(member, "serviceAccount:")
				if ok && !d.mayRead(sc.name, account) {
					stale = append(stale, reader{sc.id, account})
				}
			}
		}
	}
	return stale, nil
}

// mayRead reports whether the spec lets an account read a secret.
func (d *planData) mayRead(secret, account string) bool {
	if slices.Contains(d.opts.SecretReaders, account) {
		return true
	}
	return slices.ContainsFunc(d.services, func(sv service) bool {
		return sv.account == account && slices.Contains(sv.svc.Secrets, secret)
	})
}

const secretAccessorRole = "roles/secretmanager.secretAccessor"

// revokeAccess takes an account's right to read one secret away.
func revokeAccess(ctx context.Context, g gcloud, secretID, account string) error {
	g.env.Logf("$ gcloud secrets remove-iam-policy-binding %s (secretAccessor for %s)", secretID, account)
	err := g.run(ctx, nil, io.Discard, "secrets", "remove-iam-policy-binding", secretID,
		"--member", "serviceAccount:"+account, "--role", secretAccessorRole)
	if err != nil {
		return fmt.Errorf("taking %s's access to secret %s away failed: %w", account, secretID, err)
	}
	return nil
}

// grantAccess lets the account a service runs as read one secret. The
// binding is idempotent, so every apply can make sure it exists.
func grantAccess(ctx context.Context, g gcloud, secretID, account string) error {
	g.env.Logf("$ gcloud secrets add-iam-policy-binding %s (secretAccessor for %s)", secretID, account)
	err := g.run(ctx, nil, io.Discard, "secrets", "add-iam-policy-binding", secretID,
		"--member", "serviceAccount:"+account, "--role", secretAccessorRole)
	if err != nil {
		return fmt.Errorf("letting %s read secret %s failed: %w", account, secretID, err)
	}
	return nil
}

// dockerLogin lets docker push to Artifact Registry with a short-lived token.
func dockerLogin(ctx context.Context, g gcloud, o Options) error {
	token, err := g.output(ctx, "auth", "print-access-token")
	if err != nil || token == "" {
		return fmt.Errorf("couldn't get an access token from gcloud: %v", err)
	}
	g.env.Logf("$ docker login %s", o.registryHost())
	err = g.env.Exec(ctx, adapter.ExecOptions{Dir: g.env.Dir, Stdin: strings.NewReader(token), Stdout: io.Discard},
		"docker", "login", "--username", "oauth2accesstoken", "--password-stdin", "https://"+o.registryHost())
	if err != nil {
		return fmt.Errorf("docker login to %s failed: %w", o.registryHost(), err)
	}
	return nil
}
