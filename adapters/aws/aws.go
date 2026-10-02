// Package aws deploys a spec to Amazon ECS Express Mode through the aws CLI.
//
// Each spec service becomes the Express Mode service <spec>-<service> in one
// ECS cluster; AWS provisions its load balancer, HTTPS endpoint and
// autoscaling. Services are found by name, since anyship keeps no state.
// Services built from source are built locally, pushed to an ECR repository
// you provide and deployed by digest. Secrets live in Secrets Manager.
package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/dockerfile"
	"github.com/j75689/anyship/internal/shellwords"
	"github.com/j75689/anyship/spec"
)

// Name is the target name used in `--target` and `targets.aws`.
const Name = "aws"

const (
	projectTag  = "anyship-project"
	serviceTag  = "anyship-service"
	defaultPort = 80
	// Express Mode checks /ping by default, which few apps serve.
	defaultHealthPath = "/"

	defaultCluster            = "default"
	defaultExecutionRole      = "ecsTaskExecutionRole"
	defaultInfrastructureRole = "ecsInfrastructureRoleForExpressServices"
)

// Options is the targets.aws block of a spec.
type Options struct {
	Region  string `json:"region"`
	Profile string `json:"profile,omitempty"`
	// Cluster is an existing ECS cluster; "default" when empty.
	Cluster string `json:"cluster,omitempty"`
	// Repository is an existing ECR repository, needed when a service builds
	// from source. Each service's image is tagged with its service name.
	Repository string `json:"repository,omitempty"`
	// Roles are IAM role names or ARNs; the names AWS's Express Mode guide
	// uses are the defaults.
	ExecutionRole      string `json:"executionRole,omitempty"`
	InfrastructureRole string `json:"infrastructureRole,omitempty"`
	TaskRole           string `json:"taskRole,omitempty"`
	// Subnets and SecurityGroups place the services in your own VPC instead of
	// the default VPC's public subnets.
	Subnets        []string `json:"subnets,omitempty"`
	SecurityGroups []string `json:"securityGroups,omitempty"`
	// CPU and Memory are per task, as Express Mode takes them ("1024", "2048").
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	// MaxTasks caps autoscaling; at least the service's replicas.
	MaxTasks int `json:"maxTasks,omitempty"`
}

var (
	regionRe      = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	clusterRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
	repositoryRe  = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	roleNameRe    = regexp.MustCompile(`^[\w+=,.@-]{1,64}$`)
	roleARNRe     = regexp.MustCompile(`^arn:aws[a-z-]*:iam::[0-9]{12}:role/(?:[\w+=,.@-]+/)*([\w+=,.@-]{1,64})$`)
	subnetRe      = regexp.MustCompile(`^subnet-[0-9a-f]+$`)
	groupRe       = regexp.MustCompile(`^sg-[0-9a-f]+$`)
	profileRe     = regexp.MustCompile(`^[\w.@-]+$`)
	serviceNameRe = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	sizeRe        = regexp.MustCompile(`^[0-9]+$`)
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
	return "Amazon ECS Express Mode (HTTP containers, Secrets Manager) via the aws CLI"
}

// service is one Express Mode service to deploy.
type service struct {
	name    string // spec service name
	express string // ECS service name
	svc     *spec.Service
	port    int
	argv    []string
	health  string
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
	id       string // Secrets Manager secret name
	generate bool
}

// planData is handed from Plan to Apply.
type planData struct {
	opts     Options
	project  string
	services []service
	secrets  []secret
}

func expressName(project, service string) string { return project + "-" + service }
func secretName(project, name string) string     { return "anyship/" + project + "/" + name }

func (o Options) cluster() string {
	if o.Cluster == "" {
		return defaultCluster
	}
	return o.Cluster
}

// roleName returns the IAM role name of a name-or-ARN option.
func roleName(value, fallback string) string {
	if value == "" {
		return fallback
	}
	if m := roleARNRe.FindStringSubmatch(value); m != nil {
		return m[1]
	}
	return value
}

func (a *Adapter) Plan(_ context.Context, s *spec.Spec, env *adapter.Env) (*adapter.Plan, error) {
	plan := &adapter.Plan{Target: Name}
	opts, err := decodeOptions(s.Targets[Name])
	if err != nil {
		plan.Findings = append(plan.Findings, adapter.Finding{
			Level: adapter.Error, Code: "AWS_BAD_OPTIONS", Message: "targets.aws: " + err.Error(),
			Hint: `Set spec.targets.aws, e.g. {"region": "us-east-1", "repository": "apps"}.`,
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
				Level: adapter.Warning, Code: "AWS_DEPENDS_ON", Service: name,
				Message: "Express Mode services reach each other by URL, not by service name; dependsOn only orders the deploys.",
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
		sc := secret{name: name, id: secretName(s.Name, name), generate: s.Secrets[name].Generate != ""}
		data.secrets = append(data.secrets, sc)
		detail := "read from $" + name + " when set; otherwise the existing value in Secrets Manager is kept"
		if sc.generate {
			detail = "generated if it doesn't exist yet, then kept (set $" + name + " to override)"
		}
		plan.Actions = append(plan.Actions, adapter.Action{Op: adapter.OpNote, Kind: "Secrets Manager secret", Name: sc.id, Detail: detail})
	}
	if len(data.secrets) > 0 {
		plan.Findings = append(plan.Findings, adapter.Finding{
			Level: adapter.Warning, Code: "AWS_SECRETS_NEED_ROLE_ACCESS",
			Message: fmt.Sprintf("Secrets reach containers as environment variables, read by the task execution role %s, which needs secretsmanager:GetSecretValue on them.",
				roleName(opts.ExecutionRole, defaultExecutionRole)),
			Hint: "AmazonECSTaskExecutionRolePolicy doesn't include it; add an inline policy allowing secretsmanager:GetSecretValue on arn:aws:secretsmanager:*:*:secret:anyship/" + s.Name + "/*.",
		})
	}

	for _, sv := range data.services {
		if sv.build != nil {
			plan.Actions = append(plan.Actions, adapter.Action{Op: adapter.OpRun, Kind: "image", Name: opts.Repository + ":" + sv.express, Detail: "docker buildx build --push to ECR, deployed by digest"})
			if sv.build.generated != nil {
				plan.Files = append(plan.Files, adapter.File{Path: filepath.Join(env.OutDir, sv.name+".Dockerfile"), Contents: sv.build.generated.Dockerfile})
			}
		}
		image := sv.image
		if image == "" {
			image = "<account>.dkr.ecr." + opts.Region + ".amazonaws.com/" + opts.Repository + "@<digest from the build>"
		}
		secretARNs := map[string]string{}
		for _, name := range sv.svc.Secrets {
			secretARNs[name] = "<arn of " + secretName(s.Name, name) + ">"
		}
		args := createArgs(data, sv, image, secretARNs, "<account>", "aws")
		plan.Actions = append(plan.Actions, adapter.Action{Op: adapter.OpDeploy, Kind: "ECS Express Mode service", Name: sv.express, Detail: fmt.Sprintf("in cluster %s, %s", opts.cluster(), opts.Region)})
		plan.Files = append(plan.Files, adapter.File{
			Path: filepath.Join(env.OutDir, sv.name+".aws.txt"),
			Contents: []byte("# Generated by anyship: how this service is created. When it already exists,\n" +
				"# anyship runs update-express-gateway-service with the same settings instead.\naws " + quoteArgs(args) + "\n"),
		})
	}
	plan.Data = data
	return plan, nil
}

func checkService(name string, svc *spec.Service, s *spec.Spec, opts *Options, dir string) (service, []adapter.Finding) {
	sv := service{name: name, express: expressName(s.Name, name), svc: svc, port: defaultPort, health: defaultHealthPath}
	var findings []adapter.Finding
	add := func(level adapter.Level, code, message, hint string) {
		findings = append(findings, adapter.Finding{Level: level, Code: code, Service: name, Message: message, Hint: hint})
	}

	if !serviceNameRe.MatchString(sv.express) {
		add(adapter.Error, "AWS_NAME", fmt.Sprintf("The service name %q is longer than 63 characters, which its URL can't hold.", sv.express),
			"Shorten the spec or service name.")
	}
	switch svc.Kind {
	case spec.KindStatic:
		add(adapter.Error, "AWS_STATIC", "Static sites aren't deployed to ECS by anyship.", "Deploy the site to the cloudflare target.")
	case spec.KindWorker:
		add(adapter.Error, "AWS_WORKER", "Background workers aren't supported on the aws target yet.", "Run the worker on the vps target for now.")
	}
	if len(svc.Volumes) > 0 {
		add(adapter.Error, "AWS_VOLUMES", "Express Mode services have no persistent volumes.", "Store data in an external database or bucket, or use the vps target.")
	}
	if len(svc.Cron) > 0 {
		add(adapter.Error, "AWS_CRON", "Cron schedules aren't supported on the aws target yet.", "")
	}

	var http []spec.Port
	for _, p := range svc.Ports {
		if p.Protocol != spec.ProtocolHTTP {
			add(adapter.Error, "AWS_NON_HTTP_PORT", fmt.Sprintf("Port %d uses %s; Express Mode only serves HTTP.", p.Port, p.Protocol), "Use the vps target for raw TCP or UDP.")
			continue
		}
		http = append(http, p)
	}
	switch {
	case len(http) > 1:
		add(adapter.Error, "AWS_MULTIPLE_PORTS", "An Express Mode service sends traffic to one container port.", "Keep one HTTP port, or split the service.")
	case len(http) == 1:
		sv.port = http[0].Port
		if http[0].Exposure == spec.ExposureInternal && len(opts.Subnets) == 0 {
			add(adapter.Error, "AWS_INTERNAL_NEEDS_SUBNETS", "Without subnets, Express Mode puts services in the default VPC's public subnets, so an internal port would be public.",
				"Set spec.targets.aws.subnets to private subnets.")
		}
	case svc.Kind == spec.KindServer:
		add(adapter.Info, "AWS_PORT_ASSUMED", fmt.Sprintf("No port in the spec; the load balancer will send traffic to port %d.", defaultPort), "Add a port if the app listens elsewhere.")
	}
	if svc.HealthCheck != nil && svc.HealthCheck.Path != "" {
		sv.health = svc.HealthCheck.Path
	} else if svc.Kind == spec.KindServer {
		add(adapter.Info, "AWS_HEALTH_CHECK", "The load balancer checks / for HTTP 200 before sending traffic.", "Set healthCheck.path if / doesn't answer 200.")
	}

	for _, resourceName := range svc.Uses {
		if s.Resources[resourceName].External {
			add(adapter.Info, "AWS_EXTERNAL_RESOURCE", fmt.Sprintf("Resource %q is external; pass its connection settings through env or secrets.", resourceName), "")
		} else {
			add(adapter.Error, "AWS_RESOURCE", fmt.Sprintf("anyship doesn't provision %s on AWS (resource %q).", s.Resources[resourceName].Type, resourceName),
				"Create it yourself (for example RDS) and mark the resource external.")
		}
	}

	if svc.Start != "" {
		argv, err := shellwords.Split(svc.Start)
		if err != nil {
			add(adapter.Error, "AWS_BAD_START", "start "+err.Error()+".", "")
		}
		sv.argv = argv
	} else if svc.Entry != "" {
		add(adapter.Info, "AWS_ENTRY_IGNORED", "entry is only used by edge runtimes; the image's own command runs on ECS.", "")
	}

	if svc.Image != "" {
		sv.image = svc.Image
		return sv, findings
	}
	if opts.Repository == "" {
		add(adapter.Error, "AWS_NO_REPOSITORY", "This service builds from source, which needs an ECR repository to push to.",
			fmt.Sprintf("Create one (aws ecr create-repository --repository-name apps --region %s) and set spec.targets.aws.repository.", opts.Region))
		return sv, findings
	}
	contextDir := filepath.Join(dir, svc.Path)
	sv.build = &build{contextDir: contextDir}
	if svc.Dockerfile != "" {
		path := filepath.Join(contextDir, svc.Dockerfile)
		if _, err := os.Stat(path); err != nil {
			add(adapter.Error, "AWS_DOCKERFILE_MISSING", fmt.Sprintf("Dockerfile %s not found.", filepath.Join(svc.Path, svc.Dockerfile)), "")
		}
		sv.build.dockerfile = path
		return sv, findings
	}
	generated, err := dockerfile.Generate(svc, contextDir)
	if err != nil {
		add(adapter.Error, "AWS_NEEDS_IMAGE", "No image or Dockerfile, and anyship can't generate one: "+err.Error()+".",
			"Add a Dockerfile and set services.<name>.dockerfile, or set services.<name>.image.")
		return sv, findings
	}
	sv.build.generated = generated
	add(adapter.Info, "AWS_GENERATED_DOCKERFILE", "No Dockerfile, so anyship generated one; review it in the plan's generated files (a deploy writes them under .anyship/).",
		"To customize the build, commit your own Dockerfile and set services.<name>.dockerfile.")
	return sv, findings
}

type nameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type secretRef struct {
	Name      string `json:"name"`
	ValueFrom string `json:"valueFrom"`
}

// container is Express Mode's primaryContainer. Environment and secrets are
// always sent, so removing them from the spec removes them from the service.
type container struct {
	Image         string      `json:"image"`
	ContainerPort int         `json:"containerPort"`
	Command       []string    `json:"command,omitempty"`
	Environment   []nameValue `json:"environment"`
	Secrets       []secretRef `json:"secrets"`
}

func primaryContainer(sv service, image string, secretARNs map[string]string) string {
	c := container{Image: image, ContainerPort: sv.port, Command: sv.argv, Environment: []nameValue{}, Secrets: []secretRef{}}
	for _, k := range sortedKeys(sv.svc.Env) {
		c.Environment = append(c.Environment, nameValue{k, sv.svc.Env[k]})
	}
	for _, name := range sv.svc.Secrets {
		c.Secrets = append(c.Secrets, secretRef{name, secretARNs[name]})
	}
	return mustJSON(c)
}

// settingArgs are the flags create and update share.
func settingArgs(d *planData, sv service, image string, secretARNs map[string]string, account, partition string) []string {
	o := d.opts
	maxTasks := max(o.MaxTasks, sv.svc.Replicas)
	args := []string{
		"--primary-container", primaryContainer(sv, image, secretARNs),
		"--execution-role-arn", roleARN(o.ExecutionRole, defaultExecutionRole, account, partition),
		"--health-check-path", sv.health,
		"--scaling-target", mustJSON(map[string]int{"minTaskCount": sv.svc.Replicas, "maxTaskCount": maxTasks}),
	}
	if o.TaskRole != "" {
		args = append(args, "--task-role-arn", roleARN(o.TaskRole, "", account, partition))
	}
	if len(o.Subnets) > 0 {
		args = append(args, "--network-configuration", mustJSON(map[string][]string{"subnets": o.Subnets, "securityGroups": nonNil(o.SecurityGroups)}))
	}
	if o.CPU != "" {
		args = append(args, "--cpu", o.CPU)
	}
	if o.Memory != "" {
		args = append(args, "--memory", o.Memory)
	}
	return args
}

func createArgs(d *planData, sv service, image string, secretARNs map[string]string, account, partition string) []string {
	args := []string{"ecs", "create-express-gateway-service", "--service-name", sv.express, "--cluster", d.opts.cluster(),
		"--infrastructure-role-arn", roleARN(d.opts.InfrastructureRole, defaultInfrastructureRole, account, partition),
		"--tags", mustJSON([]map[string]string{{"key": projectTag, "value": d.project}, {"key": serviceTag, "value": sv.name}})}
	return append(args, settingArgs(d, sv, image, secretARNs, account, partition)...)
}

func updateArgs(d *planData, sv service, arn, image string, secretARNs map[string]string, account, partition string) []string {
	args := []string{"ecs", "update-express-gateway-service", "--service-arn", arn}
	return append(args, settingArgs(d, sv, image, secretARNs, account, partition)...)
}

// roleARN turns a role option into an ARN in the caller's account.
func roleARN(value, fallback, account, partition string) string {
	if strings.HasPrefix(value, "arn:") {
		return value
	}
	return fmt.Sprintf("arn:%s:iam::%s:role/%s", partition, account, roleName(value, fallback))
}

func mustJSON(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		panic(err) // only plain structs, maps and slices are encoded
	}
	return string(out)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
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
		return nil, errors.New("region is required")
	}
	opts := &Options{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(opts); err != nil {
		return nil, err
	}
	switch {
	case !regionRe.MatchString(opts.Region):
		return nil, fmt.Errorf("region %q is not a valid AWS region such as us-east-1", opts.Region)
	case opts.Profile != "" && !profileRe.MatchString(opts.Profile):
		return nil, fmt.Errorf("profile %q is not a valid profile name", opts.Profile)
	case opts.Cluster != "" && !clusterRe.MatchString(opts.Cluster):
		return nil, fmt.Errorf("cluster %q is not a valid ECS cluster name", opts.Cluster)
	case opts.Repository != "" && !repositoryRe.MatchString(opts.Repository):
		return nil, fmt.Errorf("repository %q is not a valid ECR repository name", opts.Repository)
	case opts.MaxTasks < 0:
		return nil, errors.New("maxTasks can't be negative")
	case len(opts.SecurityGroups) > 0 && len(opts.Subnets) == 0:
		return nil, errors.New("securityGroups needs subnets")
	case opts.CPU != "" && !sizeRe.MatchString(opts.CPU), opts.Memory != "" && !sizeRe.MatchString(opts.Memory):
		return nil, errors.New(`cpu and memory are numbers as strings, like "1024"`)
	}
	for field, value := range map[string]string{"executionRole": opts.ExecutionRole, "infrastructureRole": opts.InfrastructureRole, "taskRole": opts.TaskRole} {
		if value != "" && !roleNameRe.MatchString(value) && !roleARNRe.MatchString(value) {
			return nil, fmt.Errorf("%s %q is not an IAM role name or ARN", field, value)
		}
	}
	if i := slices.IndexFunc(opts.Subnets, func(s string) bool { return !subnetRe.MatchString(s) }); i >= 0 {
		return nil, fmt.Errorf("subnet %q is not a subnet id", opts.Subnets[i])
	}
	if i := slices.IndexFunc(opts.SecurityGroups, func(s string) bool { return !groupRe.MatchString(s) }); i >= 0 {
		return nil, fmt.Errorf("security group %q is not a security group id", opts.SecurityGroups[i])
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
