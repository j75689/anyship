package gcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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

// gcloud runs gcloud commands for one project.
type gcloud struct {
	env     *adapter.Env
	project string
}

// run executes gcloud with --project and --quiet; stdout is captured when out
// is set and stdin is fed from in.
func (g gcloud) run(ctx context.Context, in io.Reader, out io.Writer, args ...string) error {
	args = append(args, "--project", g.project, "--quiet")
	return g.env.Exec(ctx, adapter.ExecOptions{Dir: g.env.Dir, Stdin: in, Stdout: out}, "gcloud", args...)
}

func (g gcloud) output(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	err := g.run(ctx, nil, &out, args...)
	return strings.TrimSpace(out.String()), err
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

	g := gcloud{env: env, project: data.opts.Project}
	env.Logf("checking project %s in %s", data.opts.Project, data.opts.Region)
	checks := preflight(ctx, g, data)
	result := func(ok bool, messages ...string) *adapter.Result {
		return &adapter.Result{OK: ok, Findings: checks, Messages: messages}
	}
	if adapter.HasErrors(checks) {
		return result(false, "Preflight checks failed; nothing was changed."), nil
	}
	if env.DryRun {
		return result(true, fmt.Sprintf("Dry run: preflight checks passed for %s; nothing was changed.", data.opts.Project)), nil
	}

	for _, sc := range data.secrets {
		if err := ensureSecret(ctx, g, data.project, sc); err != nil {
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

	messages := []string{fmt.Sprintf("Deployed %s to Cloud Run in %s/%s.", data.project, data.opts.Project, data.opts.Region)}
	for _, sv := range data.services {
		env.Logf("$ gcloud run deploy %s", sv.cloudRun)
		if err := g.env.Exec(ctx, adapter.ExecOptions{Dir: env.Dir}, "gcloud", deployArgs(data, sv, sv.image)...); err != nil {
			return result(false, fmt.Sprintf("gcloud run deploy %s failed: %v", sv.cloudRun, err)), nil
		}
		if url, err := g.output(ctx, "run", "services", "describe", sv.cloudRun, "--region", data.opts.Region, "--format", "value(status.url)"); err == nil && url != "" {
			messages = append(messages, fmt.Sprintf("%s: %s", sv.name, url))
		}
	}
	return result(true, messages...), nil
}

var requiredAPIs = map[string]string{
	"run":      "run.googleapis.com",
	"registry": "artifactregistry.googleapis.com",
	"secrets":  "secretmanager.googleapis.com",
}

// preflight checks the login, project, APIs and registry before anything changes.
func preflight(ctx context.Context, g gcloud, d *planData) []adapter.Finding {
	var findings []adapter.Finding
	add := func(level adapter.Level, code, message, hint string) {
		findings = append(findings, adapter.Finding{Level: level, Code: code, Message: message, Hint: hint})
	}

	if _, err := g.output(ctx, "auth", "print-access-token"); err != nil {
		add(adapter.Error, "GCP_PREFLIGHT_AUTH", "gcloud isn't logged in.", "Run `gcloud auth login`.")
		return findings
	}
	enabled, err := g.output(ctx, "services", "list", "--enabled", "--format", "value(config.name)")
	if err != nil {
		add(adapter.Error, "GCP_PREFLIGHT_PROJECT", fmt.Sprintf("Couldn't read project %s; check that it exists and that you can access it.", d.opts.Project), "")
		return findings
	}
	building := slices.ContainsFunc(d.services, func(sv service) bool { return sv.build != nil })
	needed := []string{requiredAPIs["run"]}
	if building {
		needed = append(needed, requiredAPIs["registry"])
	}
	if len(d.secrets) > 0 {
		needed = append(needed, requiredAPIs["secrets"])
	}
	apis := strings.Fields(enabled)
	for _, api := range needed {
		if !slices.Contains(apis, api) {
			add(adapter.Error, "GCP_PREFLIGHT_API", fmt.Sprintf("The %s API isn't enabled in %s.", api, d.opts.Project),
				fmt.Sprintf("gcloud services enable %s --project %s", api, d.opts.Project))
		}
	}

	if building {
		format, err := g.output(ctx, "artifacts", "repositories", "describe", d.opts.Repository, "--location", d.opts.Region, "--format", "value(format)")
		switch {
		case err != nil:
			add(adapter.Error, "GCP_PREFLIGHT_REPOSITORY", fmt.Sprintf("Artifact Registry repository %s doesn't exist in %s.", d.opts.Repository, d.opts.Region),
				fmt.Sprintf("gcloud artifacts repositories create %s --repository-format docker --location %s --project %s", d.opts.Repository, d.opts.Region, d.opts.Project))
		case format != "DOCKER":
			add(adapter.Error, "GCP_PREFLIGHT_REPOSITORY", fmt.Sprintf("Artifact Registry repository %s holds %s packages, not Docker images.", d.opts.Repository, format), "")
		}
		if err := g.env.Exec(ctx, adapter.ExecOptions{Dir: g.env.Dir, Stdout: io.Discard}, "docker", "buildx", "version"); err != nil {
			add(adapter.Error, "GCP_PREFLIGHT_DOCKER", "Building from source needs Docker with buildx on this machine.", "Install Docker Desktop or the buildx plugin, or set services.<name>.image.")
		}
	}
	if !adapter.HasErrors(findings) {
		add(adapter.Info, "GCP_PREFLIGHT_OK", fmt.Sprintf("Project %s is ready: logged in and the required APIs are enabled.", d.opts.Project), "")
	}
	return findings
}

// ensureSecret makes the secret exist with the right value: a value from the
// deployer's environment always wins, a missing generated secret gets a random
// value, and an existing secret is otherwise left alone.
func ensureSecret(ctx context.Context, g gcloud, project string, sc secret) error {
	value, fromEnv := g.env.LookupEnv(sc.name)
	fromEnv = fromEnv && value != ""
	exists := g.run(ctx, nil, io.Discard, "secrets", "describe", sc.id) == nil

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
	err := g.run(ctx, strings.NewReader(value), io.Discard, "secrets", "create", sc.id,
		"--replication-policy", "automatic", "--data-file", "-", "--labels", projectLabel+"="+project)
	if err != nil {
		return fmt.Errorf("creating secret %s failed: %w", sc.id, err)
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
