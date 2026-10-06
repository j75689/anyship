// Package gcp deploys a spec to Google Cloud Run through the gcloud CLI.
//
// Each spec service becomes a Cloud Run service named <spec>-<service> and
// labeled anyship-project=<spec>; that label is how status, logs and destroy
// find it, since anyship keeps no state. Services built from source are built
// locally and pushed to an Artifact Registry repository you provide, then
// deployed by digest. Secrets live in Secret Manager and reach the container
// as environment variables.
package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/dockerfile"
	"github.com/j75689/anyship/image"
	"github.com/j75689/anyship/internal/shellwords"
	"github.com/j75689/anyship/internal/yamljson"
	"github.com/j75689/anyship/spec"
)

// Name is the target name used in `--target` and `targets.gcp`.
const Name = "gcp"

const (
	projectLabel = "anyship-project"
	serviceLabel = "anyship-service"
	defaultPort  = 8080
)

// Options is the targets.gcp block of a spec.
type Options struct {
	Project string `json:"project"`
	Region  string `json:"region"`
	// Repository is an existing Artifact Registry Docker repository in Region,
	// needed when a service builds from source.
	Repository string `json:"repository,omitempty"`
	// Private makes public services require authentication.
	Private bool `json:"private,omitempty"`
	// Configuration is the gcloud configuration to use (`gcloud config
	// configurations list`); the active one when empty.
	Configuration string `json:"configuration,omitempty"`
	// ServiceAccount is the email of the account services run as; the
	// project's Compute Engine default service account when empty.
	ServiceAccount string `json:"serviceAccount,omitempty"`
	// Timeout is how long a request may take, such as "10m", up to an hour.
	// When empty a deploy keeps the service's current timeout (5 minutes
	// for a new service).
	Timeout string `json:"timeout,omitempty"`
}

// maxTimeout is Cloud Run's longest request timeout.
const maxTimeout = time.Hour

// The startup probe made from healthCheck.path asks every probePeriod seconds
// and gives up after probeFailures tries: 4 minutes, which is also how long
// Cloud Run's default check lets a container take to start.
const (
	probePeriod   = 10
	probeTimeout  = 5
	probeFailures = 24
)

// timeoutSeconds is Timeout in seconds; decodeOptions has checked it.
func (o Options) timeoutSeconds() int {
	d, _ := time.ParseDuration(o.Timeout)
	return int(d / time.Second)
}

var (
	projectRe       = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	regionRe        = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)
	repositoryRe    = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	cloudRunNameRe  = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,47}[a-z0-9])?$`)
	configurationRe = regexp.MustCompile(`^[a-z][-a-z0-9]*$`)
	accountRe       = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]@[a-z0-9.-]+\.iam\.gserviceaccount\.com$|^[0-9]+-compute@developer\.gserviceaccount\.com$`)
)

type Adapter struct{}

var (
	_ adapter.Adapter      = (*Adapter)(nil)
	_ adapter.LogReader    = (*Adapter)(nil)
	_ adapter.StatusReader = (*Adapter)(nil)
	_ adapter.Destroyer    = (*Adapter)(nil)
)

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return Name }
func (*Adapter) Description() string {
	return "Google Cloud Run (HTTP containers, Secret Manager) via gcloud"
}

// service is one Cloud Run service to deploy.
type service struct {
	name     string // spec service name
	cloudRun string // Cloud Run service name
	svc      *spec.Service
	port     int
	internal bool
	argv     []string
	// image is the image to deploy; for builds it is filled in by Apply.
	image string
	build *build
}

type build struct {
	contextDir string
	// dockerfile is the user's Dockerfile, or empty when generated is set.
	dockerfile string
	generated  *dockerfile.Result
}

type secret struct {
	name     string // spec secret name, the env var in the container
	id       string // Secret Manager secret id
	generate bool
}

// planData is handed from Plan to Apply.
type planData struct {
	opts     Options
	project  string
	services []service
	secrets  []secret
}

func cloudRunName(project, service string) string { return project + "-" + service }
func secretID(project, name string) string        { return project + "-" + name }

func (o Options) registryHost() string { return o.Region + "-docker.pkg.dev" }
func (o Options) imageRepository(name string) string {
	return fmt.Sprintf("%s/%s/%s/%s", o.registryHost(), o.Project, o.Repository, name)
}

func (a *Adapter) Plan(_ context.Context, s *spec.Spec, env *adapter.Env) (*adapter.Plan, error) {
	plan := &adapter.Plan{Target: Name}
	opts, err := decodeOptions(s.Targets[Name])
	if err != nil {
		plan.Findings = append(plan.Findings, adapter.Finding{
			Level: adapter.Error, Code: "GCP_BAD_OPTIONS", Message: "spec.targets.gcp: " + err.Error(),
			Hint: adapter.OptionsHint(Name, "`project: my-project` and `region: us-central1`, plus `repository: apps` to build from source"),
		})
		return plan, nil
	}

	data := &planData{opts: *opts, project: s.Name}
	used := map[string]bool{}
	dependsNoted := false
	for _, name := range s.DeployOrder() {
		svc := s.Services[name]
		sv, findings := checkService(name, svc, s, opts, env.Dir)
		plan.Findings = append(plan.Findings, findings...)
		if len(svc.DependsOn) > 0 && !dependsNoted {
			dependsNoted = true
			plan.Findings = append(plan.Findings, adapter.Finding{
				Level: adapter.Warning, Code: "GCP_DEPENDS_ON", Service: name,
				Message: "Cloud Run services reach each other by URL, not by service name; dependsOn only orders the deploys.",
				Hint:    "Pass the other service's URL in env (anyship status shows it after the first deploy).",
			})
		}
		for _, secretName := range svc.Secrets {
			used[secretName] = true
		}
		data.services = append(data.services, sv)
	}
	if adapter.HasErrors(plan.Findings) {
		return plan, nil
	}

	for _, name := range sortedKeys(used) {
		sc := secret{name: name, id: secretID(s.Name, name), generate: s.Secrets[name].Generate != ""}
		data.secrets = append(data.secrets, sc)
		detail := "read from $" + name + " when set; otherwise the existing value in Secret Manager is kept"
		if sc.generate {
			detail = "generated if it doesn't exist yet, then kept (set $" + name + " to override)"
		}
		plan.Actions = append(plan.Actions, adapter.Action{Op: adapter.OpNote, Kind: "Secret Manager secret", Name: sc.id, Detail: detail})
	}
	if len(data.secrets) > 0 {
		plan.Findings = append(plan.Findings, adapter.Finding{
			Level: adapter.Info, Code: "GCP_SECRETS_AS_ENV",
			Message: "Secrets reach Cloud Run containers as environment variables named after the secret, not as files under /run/secrets.",
		})
		account := opts.ServiceAccount
		if account == "" {
			account = "the Compute Engine default service account"
		}
		plan.Actions = append(plan.Actions, adapter.Action{Op: adapter.OpNote, Kind: "IAM binding", Name: "roles/secretmanager.secretAccessor",
			Detail: "lets " + account + ", which the services run as, read each of these secrets (and no others)"})
	}

	for _, sv := range data.services {
		if sv.build != nil {
			plan.Actions = append(plan.Actions, adapter.Action{Op: adapter.OpRun, Kind: "image", Name: opts.imageRepository(sv.cloudRun), Detail: "docker buildx build --push, deployed by digest"})
			if sv.build.generated != nil {
				plan.Files = append(plan.Files, adapter.File{Path: filepath.Join(env.OutDir, sv.name+".Dockerfile"), Contents: sv.build.generated.Dockerfile})
			}
		}
		image := sv.image
		if image == "" {
			image = opts.imageRepository(sv.cloudRun) + "@<digest from the build>"
		}
		args := deployArgs(data, sv, image)
		plan.Actions = append(plan.Actions, adapter.Action{Op: adapter.OpDeploy, Kind: "Cloud Run service", Name: sv.cloudRun, Detail: fmt.Sprintf("in %s/%s", opts.Project, opts.Region)})
		plan.Files = append(plan.Files, adapter.File{
			Path:     filepath.Join(env.OutDir, sv.name+".gcloud.txt"),
			Contents: []byte("# Generated by anyship: the deploy command for this service.\ngcloud " + quoteArgs(args) + "\n"),
		})
	}
	plan.Data = data
	return plan, nil
}

func checkService(name string, svc *spec.Service, s *spec.Spec, opts *Options, dir string) (service, []adapter.Finding) {
	sv := service{name: name, cloudRun: cloudRunName(s.Name, name), svc: svc, port: defaultPort}
	var findings []adapter.Finding
	add := func(level adapter.Level, code, message, hint string) {
		findings = append(findings, adapter.Finding{Level: level, Code: code, Service: name, Message: message, Hint: hint})
	}

	if !cloudRunNameRe.MatchString(sv.cloudRun) {
		add(adapter.Error, "GCP_NAME", fmt.Sprintf("The Cloud Run service name %q is invalid or longer than 49 characters.", sv.cloudRun),
			"Shorten the spec or service name.")
	}
	switch svc.Kind {
	case spec.KindStatic:
		add(adapter.Error, "GCP_STATIC", "Static sites aren't deployed to Cloud Run by anyship.", "Deploy the site to the cloudflare target.")
	case spec.KindWorker:
		add(adapter.Error, "GCP_WORKER", "Background workers aren't supported on the gcp target yet.", "Run the worker on the vps target for now.")
	}
	if len(svc.Volumes) > 0 {
		add(adapter.Error, "GCP_VOLUMES", "Cloud Run has no persistent disks for volumes.", "Store data in an external database or bucket, or use the vps target.")
	}
	if len(svc.Cron) > 0 {
		add(adapter.Error, "GCP_CRON", "Cron schedules aren't supported on the gcp target yet.", "")
	}
	if len(svc.Domains) > 0 {
		add(adapter.Error, "GCP_DOMAIN_UNSUPPORTED",
			fmt.Sprintf("anyship doesn't attach custom domains on Cloud Run (domains: %s); the service answers on its run.app URL.", strings.Join(svc.Domains, ", ")),
			"Map them yourself (gcloud beta run domain-mappings create) or put a load balancer in front, then drop domains from the spec.")
	}

	var http []spec.Port
	for _, p := range svc.Ports {
		if p.Protocol != spec.ProtocolHTTP {
			add(adapter.Error, "GCP_NON_HTTP_PORT", fmt.Sprintf("Port %d uses %s; Cloud Run only serves HTTP.", p.Port, p.Protocol), "Use the vps target for raw TCP or UDP.")
			continue
		}
		http = append(http, p)
	}
	switch {
	case len(http) > 1:
		add(adapter.Error, "GCP_MULTIPLE_PORTS", "Cloud Run sends traffic to one port per service.", "Keep one HTTP port, or split the service.")
	case len(http) == 1:
		sv.port, sv.internal = http[0].Port, http[0].Exposure == spec.ExposureInternal
		if sv.internal {
			add(adapter.Warning, "GCP_INTERNAL_CALLERS",
				fmt.Sprintf("Port %d is internal, so %s is deployed with internal ingress and requires authentication: a request has to arrive through one of the project's VPC networks and carry an identity token. anyship sets up neither for the services that call it, so as deployed they can't reach it.", sv.port, sv.cloudRun),
				fmt.Sprintf("For each caller: send its traffic through a VPC network (https://cloud.google.com/run/docs/securing/private-networking), grant its service account roles/run.invoker on %s, and have the app attach an identity token (https://cloud.google.com/run/docs/authenticating/service-to-service). Or make the port public and check a token in the app.", sv.cloudRun))
		}
	case svc.Kind == spec.KindServer:
		add(adapter.Info, "GCP_PORT_ASSUMED", fmt.Sprintf("No port in the spec; Cloud Run will send traffic to %d (also passed as $PORT).", defaultPort), "")
	}

	for _, resourceName := range svc.Uses {
		if s.Resources[resourceName].External {
			add(adapter.Info, "GCP_EXTERNAL_RESOURCE", fmt.Sprintf("Resource %q is external; pass its connection settings through env or secrets.", resourceName), "")
		} else {
			add(adapter.Error, "GCP_RESOURCE", fmt.Sprintf("anyship doesn't provision %s on Google Cloud (resource %q).", s.Resources[resourceName].Type, resourceName),
				"Create it yourself (for example Cloud SQL) and mark the resource external.")
		}
	}

	if svc.Start != "" {
		argv, err := shellwords.Split(svc.Start)
		if err != nil {
			add(adapter.Error, "GCP_BAD_START", "start "+err.Error()+".", "")
		}
		sv.argv = argv
	} else if svc.Entry != "" {
		add(adapter.Info, "GCP_ENTRY_IGNORED", "entry is only used by edge runtimes; the image's own command runs on Cloud Run.", "")
	}
	if svc.Replicas > 1 {
		add(adapter.Info, "GCP_REPLICAS", fmt.Sprintf("Cloud Run scales automatically; replicas keeps %d instances warm (min instances).", svc.Replicas), "")
	}
	if hc := svc.HealthCheck; hc != nil {
		switch {
		case hc.Path != "" && !strings.HasPrefix(hc.Path, "/"):
			add(adapter.Error, "GCP_BAD_HEALTH_PATH", fmt.Sprintf("healthCheck.path %q must start with a slash.", hc.Path), "")
		case hc.Path != "":
			add(adapter.Info, "GCP_HEALTH_CHECK",
				fmt.Sprintf("healthCheck.path is the startup probe: a new revision gets traffic once GET %s answers with a 2xx or 3xx status, and the deploy fails if it hasn't within %d minutes.", hc.Path, probePeriod*probeFailures/60), "")
		case hc.Command != "":
			add(adapter.Warning, "GCP_HEALTH_COMMAND_IGNORED",
				"healthCheck.command isn't applied: Cloud Run checks a service over HTTP, not by running a command in it.",
				"Set healthCheck.path to a path that answers once the service is ready.")
		}
	}

	if svc.Image != "" {
		sv.image = svc.Image
		if registry, rest := image.Registry(svc.Image); !pullable(registry) {
			add(adapter.Error, "GCP_UNPULLABLE_IMAGE",
				fmt.Sprintf("Cloud Run can't pull image %s: it pulls from Artifact Registry, gcr.io and Docker Hub, not from %s.", svc.Image, registry),
				fmt.Sprintf("Create an Artifact Registry remote repository that proxies %s and deploy %s/%s/<remote repository>/%s, or push the image to Artifact Registry.", registry, opts.registryHost(), opts.Project, rest))
		}
		if tag, moving := image.MovingTag(svc.Image); moving {
			add(adapter.Warning, "GCP_MUTABLE_TAG",
				fmt.Sprintf("image %s is deployed by the tag %q, which can move: Cloud Run runs whatever it points at when pulled, and behind a caching registry that can be an older image.", svc.Image, tag),
				fmt.Sprintf("Pin it by digest in anyship.yaml (image: name@sha256:…), or override it for one deploy: `anyship apply --image %s=<ref>`.", name))
		}
		return sv, findings
	}
	if opts.Repository == "" {
		add(adapter.Error, "GCP_NO_REPOSITORY", "This service builds from source, which needs an Artifact Registry repository to push to.",
			fmt.Sprintf("Create one (gcloud artifacts repositories create apps --repository-format docker --location %s) and set targets.gcp.repository.", opts.Region))
		return sv, findings
	}
	contextDir := filepath.Join(dir, svc.Path)
	sv.build = &build{contextDir: contextDir}
	if svc.Dockerfile != "" {
		path := filepath.Join(contextDir, svc.Dockerfile)
		if _, err := os.Stat(path); err != nil {
			add(adapter.Error, "GCP_DOCKERFILE_MISSING", fmt.Sprintf("Dockerfile %s not found.", filepath.Join(svc.Path, svc.Dockerfile)), "")
		}
		sv.build.dockerfile = path
		return sv, findings
	}
	generated, err := dockerfile.Generate(svc, contextDir)
	if err != nil {
		add(adapter.Error, "GCP_NEEDS_IMAGE", "No image or Dockerfile, and anyship can't generate one: "+err.Error()+".",
			"Add a Dockerfile and set services.<name>.dockerfile, or set services.<name>.image.")
		return sv, findings
	}
	sv.build.generated = generated
	add(adapter.Info, "GCP_GENERATED_DOCKERFILE", "No Dockerfile, so anyship generated one; review it in the plan's generated files.",
		"To customize the build, commit your own Dockerfile and set services.<name>.dockerfile.")
	return sv, findings
}

// pullable reports whether Cloud Run pulls images from a registry. It takes
// Artifact Registry, Container Registry and Docker Hub, and refuses a deploy
// from anywhere else.
func pullable(registry string) bool {
	return registry == "docker.io" || registry == "gcr.io" || strings.HasSuffix(registry, ".gcr.io") ||
		registry == "docker.pkg.dev" || strings.HasSuffix(registry, "-docker.pkg.dev")
}

// deployArgs is the gcloud command that makes the service match the spec.
// Env vars, secrets and the startup probe are always set or cleared, so
// removals in the spec take effect.
func deployArgs(d *planData, sv service, image string) []string {
	o := d.opts
	args := []string{"run", "deploy", sv.cloudRun, "--image", image, "--region", o.Region, "--project", o.Project,
		"--port", strconv.Itoa(sv.port), "--labels", fmt.Sprintf("%s=%s,%s=%s", projectLabel, d.project, serviceLabel, sv.name)}
	switch {
	case sv.internal:
		args = append(args, "--ingress", "internal", "--no-allow-unauthenticated")
	case o.Private:
		args = append(args, "--ingress", "all", "--no-allow-unauthenticated")
	default:
		args = append(args, "--ingress", "all", "--allow-unauthenticated")
	}
	if len(sv.svc.Env) > 0 {
		var pairs []string
		for _, k := range sortedKeys(sv.svc.Env) {
			pairs = append(pairs, k+"="+sv.svc.Env[k])
		}
		args = append(args, "--set-env-vars", listFlag(pairs))
	} else {
		args = append(args, "--clear-env-vars")
	}
	if len(sv.svc.Secrets) > 0 {
		var pairs []string
		for _, name := range sv.svc.Secrets {
			pairs = append(pairs, name+"="+secretID(d.project, name)+":latest")
		}
		args = append(args, "--set-secrets", listFlag(pairs))
	} else {
		args = append(args, "--clear-secrets")
	}
	if len(sv.argv) > 0 {
		args = append(args, "--command", listFlag(sv.argv[:1]))
		if len(sv.argv) > 1 {
			args = append(args, "--args", listFlag(sv.argv[1:]))
		}
	}
	if sv.svc.Replicas > 1 {
		args = append(args, "--min-instances", strconv.Itoa(sv.svc.Replicas))
	}
	if hc := sv.svc.HealthCheck; hc != nil && hc.Path != "" {
		args = append(args, "--startup-probe", listFlag([]string{
			"httpGet.path=" + hc.Path,
			fmt.Sprintf("periodSeconds=%d", probePeriod),
			fmt.Sprintf("timeoutSeconds=%d", probeTimeout),
			fmt.Sprintf("failureThreshold=%d", probeFailures),
		}))
	} else {
		args = append(args, "--startup-probe", "")
	}
	if o.ServiceAccount != "" {
		args = append(args, "--service-account", o.ServiceAccount)
	}
	if o.Timeout != "" {
		args = append(args, "--timeout", strconv.Itoa(o.timeoutSeconds()))
	}
	return append(args, "--quiet")
}

// listFlag joins values for a gcloud list flag, switching to gcloud's
// ^DELIM^ syntax when a value contains a comma.
func listFlag(values []string) string {
	if !slices.ContainsFunc(values, func(v string) bool { return strings.Contains(v, ",") }) {
		return strings.Join(values, ",")
	}
	for _, delim := range []string{"|", ";", "#", "~", "@"} {
		if !slices.ContainsFunc(values, func(v string) bool { return strings.Contains(v, delim) }) {
			return "^" + delim + "^" + strings.Join(values, delim)
		}
	}
	return strings.Join(values, ",")
}

func quoteArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellwords.Quote(a)
	}
	return strings.Join(quoted, " ")
}

func decodeOptions(raw json.RawMessage) (*Options, error) {
	if len(raw) == 0 {
		return nil, errors.New("project and region are required")
	}
	opts := &Options{}
	if err := adapter.DecodeOptions(raw, opts); err != nil {
		// gcloud's own --timeout takes a bare number of seconds, so people
		// will write one here. Quoting it, the usual advice for a number
		// where a string belongs, would still leave it without a unit.
		var problems yamljson.Problems
		if errors.As(err, &problems) {
			for i, problem := range problems {
				if problem.Path == "timeout" && !problem.Unknown {
					problems[i].Detail = `needs a unit: write a duration such as "600s" or "10m", not a bare number`
				}
			}
		}
		return nil, err
	}
	switch {
	case !projectRe.MatchString(opts.Project):
		return nil, fmt.Errorf("project %q is not a valid Google Cloud project id", opts.Project)
	case !regionRe.MatchString(opts.Region):
		return nil, fmt.Errorf("region %q is not a valid region such as us-central1", opts.Region)
	case opts.Repository != "" && !repositoryRe.MatchString(opts.Repository):
		return nil, fmt.Errorf("repository %q is not a valid Artifact Registry repository name", opts.Repository)
	case opts.Configuration != "" && !configurationRe.MatchString(opts.Configuration):
		return nil, fmt.Errorf("configuration %q is not a valid gcloud configuration name", opts.Configuration)
	case opts.ServiceAccount != "" && !accountRe.MatchString(opts.ServiceAccount):
		return nil, fmt.Errorf("serviceAccount %q is not a service account email", opts.ServiceAccount)
	}
	if opts.Timeout != "" {
		d, err := time.ParseDuration(opts.Timeout)
		if err != nil || d < time.Second || d > maxTimeout || d%time.Second != 0 {
			return nil, fmt.Errorf("timeout %q is not a whole number of seconds between 1s and 1h, such as \"10m\"", opts.Timeout)
		}
	}
	return opts, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
