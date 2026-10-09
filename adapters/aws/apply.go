package aws

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

// cli runs aws commands for one region and profile.
type cli struct {
	env  *adapter.Env
	opts Options
}

// run executes aws with the region, profile and no pager; stdout is captured
// when out is set and stdin is fed from in.
func (c cli) run(ctx context.Context, in io.Reader, out io.Writer, args ...string) error {
	args = append(args, "--region", c.opts.Region, "--no-cli-pager")
	if c.opts.Profile != "" {
		args = append(args, "--profile", c.opts.Profile)
	}
	return c.env.Exec(ctx, adapter.ExecOptions{Dir: c.env.Dir, Stdin: in, Stdout: out}, "aws", args...)
}

// json runs a command with JSON output and decodes it into v.
func (c cli) json(ctx context.Context, v any, args ...string) error {
	var out bytes.Buffer
	if err := c.run(ctx, nil, &out, append(args, "--output", "json")...); err != nil {
		return err
	}
	if err := json.Unmarshal(out.Bytes(), v); err != nil {
		return fmt.Errorf("unexpected output from aws %s: %w", strings.Join(args[:2], " "), err)
	}
	return nil
}

// identity is who the aws CLI is logged in as.
type identity struct {
	Account string `json:"Account"`
	Arn     string `json:"Arn"`
}

func (id identity) partition() string {
	if parts := strings.Split(id.Arn, ":"); len(parts) > 1 && parts[1] != "" {
		return parts[1]
	}
	return "aws"
}

func (id identity) registry(region string) string {
	host := id.Account + ".dkr.ecr." + region + ".amazonaws.com"
	if id.partition() == "aws-cn" {
		host += ".cn"
	}
	return host
}

// Apply runs the preflight checks, then sets secrets, builds and pushes
// images, and creates or updates every service. With env.DryRun it stops
// after the checks.
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

	c := cli{env: env, opts: data.opts}
	env.Logf("checking AWS in %s", data.opts.Region)
	id, checks := preflight(ctx, c, data)
	result := func(ok bool, messages ...string) *adapter.Result {
		return &adapter.Result{OK: ok, Findings: checks, Messages: messages}
	}
	if adapter.HasErrors(checks) {
		return result(false, "Preflight checks failed; nothing was changed."), nil
	}
	if env.DryRun {
		return result(true, fmt.Sprintf("Dry run: preflight checks passed for account %s in %s; nothing was changed.", id.Account, data.opts.Region)), nil
	}

	secretARNs := map[string]string{}
	for _, sc := range data.secrets {
		arn, err := ensureSecret(ctx, c, data.project, sc)
		if err != nil {
			return result(false, err.Error()), nil
		}
		secretARNs[sc.name] = arn
	}

	if slices.ContainsFunc(data.services, func(sv service) bool { return sv.build != nil }) {
		if err := dockerLogin(ctx, c, id.registry(data.opts.Region)); err != nil {
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
		ref, err := image.BuildAndPush(ctx, env, image.Build{
			Context: sv.build.contextDir, Dockerfile: path,
			Repository: id.registry(data.opts.Region) + "/" + data.opts.Repository, Tag: sv.express,
		})
		if err != nil {
			return result(false, err.Error()), nil
		}
		sv.image = ref
	}

	existing, err := describeServices(ctx, c, data.opts, expressNames(data))
	if err != nil {
		return result(false, err.Error()), nil
	}
	messages := []string{fmt.Sprintf("Deployed %s to ECS Express Mode in %s (cluster %s). New tasks roll out over a few minutes; `anyship status -t aws` shows progress.",
		data.project, data.opts.Region, data.opts.cluster())}
	for _, sv := range data.services {
		arn, err := deploy(ctx, c, data, sv, existing[sv.express], secretARNs, id)
		if err != nil {
			return result(false, err.Error()), nil
		}
		if ex, err := describeExpress(ctx, c, arn); err == nil && ex.url() != "" {
			messages = append(messages, fmt.Sprintf("%s: %s", sv.name, ex.url()))
		}
	}
	return result(true, messages...), nil
}

// deploy creates the service, or updates it when it already exists, and
// returns its ARN.
func deploy(ctx context.Context, c cli, d *planData, sv service, current *ecsService, secretARNs map[string]string, id identity) (string, error) {
	if current != nil && current.Status == "DRAINING" {
		return "", fmt.Errorf("%s is still being deleted; apply again once it's gone", sv.express)
	}
	if current != nil && current.Status == "ACTIVE" {
		c.env.Logf("$ aws ecs update-express-gateway-service %s", sv.express)
		if err := c.run(ctx, nil, io.Discard, updateArgs(d, sv, current.ServiceArn, sv.image, secretARNs, id.Account, id.partition())...); err != nil {
			return "", fmt.Errorf("updating %s failed: %w", sv.express, err)
		}
		return current.ServiceArn, nil
	}
	c.env.Logf("$ aws ecs create-express-gateway-service %s", sv.express)
	var created struct {
		Service struct {
			ServiceArn string `json:"serviceArn"`
		} `json:"service"`
	}
	if err := c.json(ctx, &created, createArgs(d, sv, sv.image, secretARNs, id.Account, id.partition())...); err != nil {
		return "", fmt.Errorf("creating %s failed: %w", sv.express, err)
	}
	return created.Service.ServiceArn, nil
}

func expressNames(d *planData) []string {
	names := make([]string, len(d.services))
	for i, sv := range d.services {
		names[i] = sv.express
	}
	return names
}

const (
	executionTrust       = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ecs-tasks.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	infrastructureTrust  = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ecs.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	executionPolicy      = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
	infrastructurePolicy = "arn:aws:iam::aws:policy/service-role/AmazonECSInfrastructureRoleforExpressGatewayServices"
)

var notInstalled = adapter.Finding{Level: adapter.Error, Code: "AWS_PREFLIGHT_CLI", Message: "The aws CLI isn't installed on this machine.",
	Hint: "Install AWS CLI v2 (https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html), then run `aws configure` or `aws sso login`."}

const loginHint = "Run `aws configure` or `aws sso login`, or set spec.targets.aws.profile."

// preflight checks the login, cluster, roles and registry before anything changes.
func preflight(ctx context.Context, c cli, d *planData) (identity, []adapter.Finding) {
	var findings []adapter.Finding
	add := func(level adapter.Level, code, message, hint string) {
		findings = append(findings, adapter.Finding{Level: level, Code: code, Message: message, Hint: hint})
	}

	var id identity
	err := c.json(ctx, &id, "sts", "get-caller-identity")
	if adapter.NotInstalled(err) {
		return id, append(findings, notInstalled)
	}
	if err != nil || id.Account == "" {
		add(adapter.Error, "AWS_PREFLIGHT_AUTH", "The aws CLI isn't logged in.", loginHint)
		return id, findings
	}

	var clusters struct {
		Clusters []struct {
			Status string `json:"status"`
		} `json:"clusters"`
	}
	cluster := d.opts.cluster()
	if err := c.json(ctx, &clusters, "ecs", "describe-clusters", "--clusters", cluster); err != nil ||
		len(clusters.Clusters) == 0 || clusters.Clusters[0].Status != "ACTIVE" {
		add(adapter.Error, "AWS_PREFLIGHT_CLUSTER", fmt.Sprintf("ECS cluster %s doesn't exist in %s.", cluster, d.opts.Region),
			fmt.Sprintf("aws ecs create-cluster --cluster-name %s --region %s", cluster, d.opts.Region))
	}

	roles := []struct{ name, trust, policy string }{
		{roleName(d.opts.ExecutionRole, defaultExecutionRole), executionTrust, executionPolicy},
		{roleName(d.opts.InfrastructureRole, defaultInfrastructureRole), infrastructureTrust, infrastructurePolicy},
	}
	if d.opts.TaskRole != "" {
		roles = append(roles, struct{ name, trust, policy string }{roleName(d.opts.TaskRole, ""), "", ""})
	}
	for _, r := range roles {
		if c.run(ctx, nil, io.Discard, "iam", "get-role", "--role-name", r.name) == nil {
			continue
		}
		hint := "Create it, or point spec.targets.aws at an existing role."
		if r.trust != "" {
			hint = fmt.Sprintf("aws iam create-role --role-name %s --assume-role-policy-document '%s' && aws iam attach-role-policy --role-name %s --policy-arn %s",
				r.name, r.trust, r.name, r.policy)
		}
		add(adapter.Error, "AWS_PREFLIGHT_ROLE", fmt.Sprintf("IAM role %s doesn't exist or can't be read.", r.name), hint)
	}

	if slices.ContainsFunc(d.services, func(sv service) bool { return sv.build != nil }) {
		if c.run(ctx, nil, io.Discard, "ecr", "describe-repositories", "--repository-names", d.opts.Repository) != nil {
			add(adapter.Error, "AWS_PREFLIGHT_REPOSITORY", fmt.Sprintf("ECR repository %s doesn't exist in %s.", d.opts.Repository, d.opts.Region),
				fmt.Sprintf("aws ecr create-repository --repository-name %s --region %s", d.opts.Repository, d.opts.Region))
		}
		if err := c.env.Exec(ctx, adapter.ExecOptions{Dir: c.env.Dir, Stdout: io.Discard}, "docker", "buildx", "version"); err != nil {
			add(adapter.Error, "AWS_PREFLIGHT_DOCKER", "Building from source needs Docker with buildx on this machine.", "Install Docker Desktop or the buildx plugin, or set services.<name>.image.")
		}
	}
	if !adapter.HasErrors(findings) {
		add(adapter.Info, "AWS_PREFLIGHT_OK", fmt.Sprintf("Account %s is ready in %s: logged in, cluster %s and the IAM roles exist.", id.Account, d.opts.Region, cluster), "")
	}
	return id, findings
}

// ensureSecret makes the secret exist with the right value and returns its
// ARN: a value from the deployer's environment always wins, a missing
// generated secret gets a random value, and an existing secret is otherwise
// left alone.
func ensureSecret(ctx context.Context, c cli, project string, sc secret) (string, error) {
	value, fromEnv := c.env.LookupEnv(sc.name)
	fromEnv = fromEnv && value != ""

	var current struct {
		ARN         string `json:"ARN"`
		DeletedDate any    `json:"DeletedDate"`
	}
	exists := c.json(ctx, &current, "secretsmanager", "describe-secret", "--secret-id", sc.id) == nil
	if exists && current.DeletedDate != nil {
		return "", fmt.Errorf("secret %s is scheduled for deletion; restore it with `aws secretsmanager restore-secret --secret-id %s` and apply again", sc.id, sc.id)
	}

	switch {
	case exists && !fromEnv:
		return current.ARN, nil
	case exists:
		c.env.Logf("$ aws secretsmanager put-secret-value %s (value from $%s)", sc.id, sc.name)
		err := withValueFile(value, func(uri string) error {
			return c.run(ctx, nil, io.Discard, "secretsmanager", "put-secret-value", "--secret-id", sc.id, "--secret-string", uri)
		})
		if err != nil {
			return "", fmt.Errorf("updating secret %s failed: %w", sc.id, err)
		}
		return current.ARN, nil
	case !fromEnv && !sc.generate:
		return "", fmt.Errorf("secret %s has no value: export %s and apply again", sc.id, sc.name)
	case !fromEnv:
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		value = hex.EncodeToString(buf)
	}
	c.env.Logf("$ aws secretsmanager create-secret %s", sc.id)
	var created struct {
		ARN string `json:"ARN"`
	}
	err := withValueFile(value, func(uri string) error {
		return c.json(ctx, &created, "secretsmanager", "create-secret", "--name", sc.id, "--secret-string", uri,
			"--tags", mustJSON([]map[string]string{{"Key": projectTag, "Value": project}}))
	})
	if err != nil {
		return "", fmt.Errorf("creating secret %s failed: %w", sc.id, err)
	}
	return created.ARN, nil
}

// withValueFile passes a secret value to the aws CLI as file://, so it never
// shows up in the process list. The file is readable only by the user and
// removed afterwards.
func withValueFile(value string, fn func(uri string) error) error {
	dir, err := os.MkdirTemp("", "anyship-secret-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "value")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		return err
	}
	return fn("file://" + path)
}

// dockerLogin lets docker push to ECR with a short-lived password.
func dockerLogin(ctx context.Context, c cli, registry string) error {
	var token bytes.Buffer
	if err := c.run(ctx, nil, &token, "ecr", "get-login-password"); err != nil || token.Len() == 0 {
		return fmt.Errorf("couldn't get an ECR login password from the aws CLI: %v", err)
	}
	c.env.Logf("$ docker login %s", registry)
	err := c.env.Exec(ctx, adapter.ExecOptions{Dir: c.env.Dir, Stdin: strings.NewReader(strings.TrimSpace(token.String())), Stdout: io.Discard},
		"docker", "login", "--username", "AWS", "--password-stdin", registry)
	if err != nil {
		return fmt.Errorf("docker login to %s failed: %w", registry, err)
	}
	return nil
}
