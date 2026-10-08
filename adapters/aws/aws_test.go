package aws

import (
	"context"
	"encoding/json"
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
	// file is the contents of a file:// argument, read when the call ran.
	file string
}

func (c call) line() string { return c.name + " " + strings.Join(c.args, " ") }

func (c call) flag(name string) string {
	if i := slices.Index(c.args, name); i >= 0 && i+1 < len(c.args) {
		return c.args[i+1]
	}
	return ""
}

// fakeAWS answers commands by prefix, like an account where everything exists.
type fakeAWS struct {
	calls  []call
	out    map[string]string
	fail   []string
	digest string
}

const account = "123456789012"

func newFakeAWS() *fakeAWS {
	return &fakeAWS{
		out: map[string]string{
			"aws sts get-caller-identity":            `{"Account": "` + account + `", "Arn": "arn:aws:iam::` + account + `:user/dev"}`,
			"aws ecs describe-clusters":              `{"clusters": [{"status": "ACTIVE"}]}`,
			"aws ecs describe-services":              `{"services": [], "failures": [{"reason": "MISSING"}]}`,
			"aws ecr get-login-password":             "ecr-password\n",
			"aws secretsmanager create-secret":       `{"ARN": "arn:aws:secretsmanager:us-east-1:` + account + `:secret:anyship/shop/SESSION-AbCdEf"}`,
			"aws secretsmanager describe-secret":     `{"ARN": "arn:aws:secretsmanager:us-east-1:` + account + `:secret:anyship/shop/TOKEN-XyZ"}`,
			"aws ecs create-express-gateway-service": `{"service": {"serviceArn": "arn:aws:ecs:us-east-1:` + account + `:service/default/shop-web"}}`,
			"aws ecs describe-express-gateway-service": `{"service": {"activeConfigurations": [{"ingressPaths": [{"accessType": "PUBLIC", "endpoint": "shop-web.ecs.us-east-1.on.aws"}],
				"primaryContainer": {"awsLogsConfiguration": {"logGroup": "/aws/ecs/default/shop-web", "logStreamPrefix": "ecs"}}}]}}`,
		},
		digest: "sha256:" + strings.Repeat("c", 64),
	}
}

func (f *fakeAWS) exec(_ context.Context, opts adapter.ExecOptions, name string, args ...string) error {
	c := call{name: name, args: args}
	if opts.Stdin != nil {
		data, err := io.ReadAll(opts.Stdin)
		if err != nil {
			return err
		}
		c.stdin = string(data)
	}
	for _, a := range args {
		if path, ok := strings.CutPrefix(a, "file://"); ok {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			c.file = string(data)
		}
	}
	f.calls = append(f.calls, c)
	line := c.line()
	for _, prefix := range f.fail {
		if strings.HasPrefix(line, prefix) {
			return errors.New("exit status 254")
		}
	}
	if name == "docker" && len(args) > 1 && args[1] == "build" {
		if err := os.WriteFile(c.flag("--metadata-file"), []byte(`{"containerimage.digest": "`+f.digest+`"}`), 0o644); err != nil {
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

func (f *fakeAWS) find(prefix string) *call {
	for i := range f.calls {
		if strings.HasPrefix(f.calls[i].line(), prefix) {
			return &f.calls[i]
		}
	}
	return nil
}

func (f *fakeAWS) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c.line(), prefix) {
			n++
		}
	}
	return n
}

func newEnv(t *testing.T, dir string, env map[string]string) (*adapter.Env, *fakeAWS) {
	t.Helper()
	fa := newFakeAWS()
	return &adapter.Env{
		Dir:    dir,
		OutDir: filepath.Join(dir, ".anyship", Name),
		Logf:   func(string, ...any) {},
		Exec:   fa.exec,
		LookupEnv: func(key string) (string, bool) {
			v, ok := env[key]
			return v, ok
		},
	}, fa
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

const target = `"targets": {"aws": {"region": "us-east-1", "repository": "apps"}}`

func nodeApp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name": "web", "scripts": {"start": "node server.js"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.js"), []byte("require('http').createServer().listen(3000)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPlanImageService(t *testing.T) {
	s := parse(t, `{"name": "shop",
		"services": {"web": {"kind": "server", "image": "nginx:1.27", "ports": [{"port": 8080}], "healthCheck": {"path": "/healthz"},
			"env": {"MODE": "prod"}, "secrets": ["API_KEY"], "replicas": 2, "start": "nginx -g 'daemon off;'"}},
		"secrets": {"API_KEY": {}}, "targets": {"aws": {"region": "us-east-1", "maxTasks": 5, "cpu": "1024"}}}`)
	env, _ := newEnv(t, t.TempDir(), nil)
	p := plan(t, s, env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if !slices.Contains(codes(p, adapter.Warning), "AWS_SECRETS_NEED_ROLE_ACCESS") {
		t.Errorf("warnings = %v", codes(p, adapter.Warning))
	}
	data := p.Data.(*planData)
	c := call{name: "aws", args: createArgs(data, data.services[0], "nginx:1.27", map[string]string{"API_KEY": "arn:key"}, account, "aws")}
	if !strings.HasPrefix(c.line(), "aws ecs create-express-gateway-service --service-name shop-web --cluster default") {
		t.Errorf("args = %s", c.line())
	}
	for flag, want := range map[string]string{
		"--infrastructure-role-arn": "arn:aws:iam::" + account + ":role/ecsInfrastructureRoleForExpressServices",
		"--execution-role-arn":      "arn:aws:iam::" + account + ":role/ecsTaskExecutionRole",
		"--health-check-path":       "/healthz",
		"--scaling-target":          `{"maxTaskCount":5,"minTaskCount":2}`,
		"--cpu":                     "1024",
		"--tags":                    `[{"key":"anyship-project","value":"shop"},{"key":"anyship-service","value":"web"}]`,
	} {
		if got := c.flag(flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	var pc container
	if err := json.Unmarshal([]byte(c.flag("--primary-container")), &pc); err != nil {
		t.Fatal(err)
	}
	if pc.Image != "nginx:1.27" || pc.ContainerPort != 8080 || !slices.Equal(pc.Command, []string{"nginx", "-g", "daemon off;"}) ||
		pc.Environment[0] != (nameValue{"MODE", "prod"}) || pc.Secrets[0] != (secretRef{"API_KEY", "arn:key"}) {
		t.Errorf("primary container = %+v", pc)
	}
}

// A service's memory and cpu are passed in ECS's units and win over the
// target's defaults.
func TestPlanRefusesServiceURLRefs(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {
		"web": {"kind": "server", "image": "web:1", "ports": [{"port": 8080}], "env": {"API": "${services.api.url}"}},
		"api": {"kind": "server", "image": "api:1", "ports": [{"port": 8080}]}}, `+target+`}`)
	env, _ := newEnv(t, t.TempDir(), nil)
	if errs := codes(plan(t, s, env), adapter.Error); !slices.Equal(errs, []string{"AWS_SERVICE_URL"}) {
		t.Errorf("errors = %v, want AWS_SERVICE_URL", errs)
	}
}

func TestPlanMemoryAndCPU(t *testing.T) {
	s := parse(t, `{"name": "shop",
		"services": {
			"api": {"kind": "server", "image": "api:1", "ports": [{"port": 8080}], "memory": "4GB", "cpu": 0.5},
			"web": {"kind": "server", "image": "web:1", "ports": [{"port": 8080}]}},
		"targets": {"aws": {"region": "us-east-1", "cpu": "1024", "memory": "2048"}}}`)
	env, _ := newEnv(t, t.TempDir(), nil)
	p := plan(t, s, env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	data := p.Data.(*planData)
	for _, sv := range data.services {
		want := map[string][2]string{"api": {"512", "4096"}, "web": {"1024", "2048"}}[sv.name]
		c := call{name: "aws", args: createArgs(data, sv, sv.image, nil, account, "aws")}
		if cpu, memory := c.flag("--cpu"), c.flag("--memory"); cpu != want[0] || memory != want[1] {
			t.Errorf("%s: --cpu %s --memory %s, want %s and %s", sv.name, cpu, memory, want[0], want[1])
		}
	}

	odd := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "web:1", "ports": [{"port": 8080}], "cpu": 3}}, `+target+`}`)
	if errs := codes(plan(t, odd, env), adapter.Error); !slices.Contains(errs, "AWS_CPU") {
		t.Errorf("cpu 3: errors = %v, want AWS_CPU", errs)
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
			"admin": {"kind": "server", "image": "admin", "ports": [{"port": 80, "exposure": "internal"}]},
			"www": {"kind": "server", "image": "www", "ports": [{"port": 80}], "domains": ["shop.example.com"]}},
		"resources": {"cache": {"type": "redis"}}, `+target+`}`)
	env, _ := newEnv(t, t.TempDir(), nil)
	errs := codes(plan(t, s, env), adapter.Error)
	for _, want := range []string{"AWS_STATIC", "AWS_WORKER", "AWS_VOLUMES", "AWS_NON_HTTP_PORT", "AWS_MULTIPLE_PORTS", "AWS_RESOURCE", "AWS_INTERNAL_NEEDS_SUBNETS", "AWS_DOMAIN_UNSUPPORTED"} {
		if !slices.Contains(errs, want) {
			t.Errorf("missing error %s in %v", want, errs)
		}
	}
}

func TestPlanOptions(t *testing.T) {
	for name, targets := range map[string]string{
		"missing":          `{}`,
		"bad region":       `{"aws": {"region": "virginia"}}`,
		"unknown field":    `{"aws": {"region": "us-east-1", "zone": "a"}}`,
		"bad subnet":       `{"aws": {"region": "us-east-1", "subnets": ["vpc-1"]}}`,
		"groups no subnet": `{"aws": {"region": "us-east-1", "securityGroups": ["sg-1"]}}`,
		"bad role":         `{"aws": {"region": "us-east-1", "executionRole": "has space"}}`,
		"bad cpu":          `{"aws": {"region": "us-east-1", "cpu": "1 vCPU"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}}, "targets": `+targets+`}`)
			env, _ := newEnv(t, t.TempDir(), nil)
			if errs := codes(plan(t, s, env), adapter.Error); !slices.Equal(errs, []string{"AWS_BAD_OPTIONS"}) {
				t.Errorf("errors = %v", errs)
			}
		})
	}
}

func TestRoleARN(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"", "arn:aws:iam::" + account + ":role/fallback"},
		{"custom", "arn:aws:iam::" + account + ":role/custom"},
		{"arn:aws:iam::999999999999:role/path/other", "arn:aws:iam::999999999999:role/path/other"},
	} {
		if got := roleARN(tc.value, "fallback", account, "aws"); got != tc.want {
			t.Errorf("roleARN(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}
	if got := roleName("arn:aws:iam::999999999999:role/path/other", "x"); got != "other" {
		t.Errorf("roleName = %q", got)
	}
}

func TestApplyBuildsAndCreates(t *testing.T) {
	dir := nodeApp(t)
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "start": "node server.js", "ports": [{"port": 3000}], "secrets": ["SESSION"]}},
		"secrets": {"SESSION": {"generate": "hex32"}}, `+target+`}`)
	env, fa := newEnv(t, dir, nil)
	fa.fail = []string{"aws secretsmanager describe-secret"}
	p := plan(t, s, env)
	if errs := codes(p, adapter.Error); len(errs) > 0 {
		t.Fatalf("plan errors: %v", errs)
	}
	res, err := New().Apply(context.Background(), p, s, env)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("apply failed: %v %+v", res.Messages, res.Findings)
	}

	for _, c := range fa.calls {
		if c.name == "aws" && (!slices.Contains(c.args, "--region") || !slices.Contains(c.args, "--no-cli-pager")) {
			t.Errorf("aws call without --region/--no-cli-pager: %s", c.line())
		}
	}
	create := fa.find("aws secretsmanager create-secret --name anyship/shop/SESSION")
	if create == nil || len(create.file) != 64 || strings.Contains(create.line(), create.file) {
		t.Fatalf("generated secret not passed through a file: %+v", create)
	}
	registry := account + ".dkr.ecr.us-east-1.amazonaws.com"
	if login := fa.find("docker login --username AWS --password-stdin " + registry); login == nil || login.stdin != "ecr-password" {
		t.Fatalf("docker login = %+v", login)
	}
	if build := fa.find("docker buildx build --platform linux/amd64"); build == nil || build.flag("--tag") != registry+"/apps:shop-web" {
		t.Fatalf("build = %+v", build)
	}
	deploy := fa.find("aws ecs create-express-gateway-service --service-name shop-web")
	if deploy == nil {
		t.Fatalf("not created; calls: %v", fa.calls)
	}
	var pc container
	_ = json.Unmarshal([]byte(deploy.flag("--primary-container")), &pc)
	if pc.Image != registry+"/apps@"+fa.digest || pc.Secrets[0].ValueFrom != "arn:aws:secretsmanager:us-east-1:"+account+":secret:anyship/shop/SESSION-AbCdEf" {
		t.Errorf("primary container = %+v", pc)
	}
	if !slices.Contains(res.Messages, "web: https://shop-web.ecs.us-east-1.on.aws") {
		t.Errorf("messages = %v", res.Messages)
	}
	if _, err := os.Stat(filepath.Join(env.OutDir, "web.aws.txt")); err != nil {
		t.Errorf("review file not written: %v", err)
	}
}

func TestApplyUpdatesAnExistingService(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx", "secrets": ["TOKEN"]}},
		"secrets": {"TOKEN": {}}, `+target+`}`)
	env, fa := newEnv(t, t.TempDir(), map[string]string{"TOKEN": "s3cret"})
	fa.out["aws ecs describe-services"] = `{"services": [{"serviceName": "shop-web", "serviceArn": "arn:svc/shop-web", "status": "ACTIVE"}]}`
	res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil || !res.OK {
		t.Fatalf("apply: %v %+v", err, res)
	}
	if put := fa.find("aws secretsmanager put-secret-value --secret-id anyship/shop/TOKEN"); put == nil || put.file != "s3cret" {
		t.Errorf("put-secret-value = %+v", put)
	}
	if fa.find("aws ecs update-express-gateway-service --service-arn arn:svc/shop-web") == nil || fa.find("aws ecs create-express-gateway-service") != nil {
		t.Errorf("should update, not create: %v", fa.calls)
	}
	if fa.find("docker") != nil {
		t.Error("image service should not touch docker")
	}
}

func TestApplyMissingSecretFails(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx", "secrets": ["TOKEN"]}},
		"secrets": {"TOKEN": {}}, `+target+`}`)
	env, fa := newEnv(t, t.TempDir(), nil)
	fa.fail = []string{"aws secretsmanager describe-secret"}
	res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
	if err != nil || res.OK {
		t.Fatalf("apply should fail: %v %+v", err, res)
	}
	if fa.find("aws ecs create-express-gateway-service") != nil {
		t.Error("deployed despite the missing secret")
	}
}

func TestApplyPreflight(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}}, `+target+`}`)

	t.Run("missing role and cluster", func(t *testing.T) {
		env, fa := newEnv(t, t.TempDir(), nil)
		fa.fail = []string{"aws iam get-role --role-name ecsInfrastructureRoleForExpressServices"}
		fa.out["aws ecs describe-clusters"] = `{"clusters": [], "failures": [{"reason": "MISSING"}]}`
		res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
		if err != nil || res.OK {
			t.Fatalf("apply should fail: %v %+v", err, res)
		}
		var got []string
		for _, f := range res.Findings {
			got = append(got, f.Code)
		}
		if !slices.Equal(got, []string{"AWS_PREFLIGHT_CLUSTER", "AWS_PREFLIGHT_ROLE"}) || !strings.Contains(res.Findings[1].Hint, "AmazonECSInfrastructureRoleforExpressGatewayServices") {
			t.Errorf("findings = %+v", res.Findings)
		}
		if fa.find("aws ecs create-express-gateway-service") != nil {
			t.Error("deployed despite failed preflight")
		}
	})

	t.Run("not logged in", func(t *testing.T) {
		env, fa := newEnv(t, t.TempDir(), nil)
		fa.fail = []string{"aws sts"}
		res, _ := New().Apply(context.Background(), plan(t, s, env), s, env)
		if res.OK || res.Findings[0].Code != "AWS_PREFLIGHT_AUTH" {
			t.Errorf("result = %+v", res)
		}
	})

	t.Run("dry run", func(t *testing.T) {
		env, fa := newEnv(t, t.TempDir(), nil)
		env.DryRun = true
		res, err := New().Apply(context.Background(), plan(t, s, env), s, env)
		if err != nil || !res.OK {
			t.Fatalf("dry run: %v %+v", err, res)
		}
		if fa.find("aws ecs create-express-gateway-service") != nil {
			t.Error("dry run deployed")
		}
	})
}

func TestStatus(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {
		"web": {"kind": "server", "image": "nginx"}, "api": {"kind": "server", "image": "api"},
		"admin": {"kind": "server", "image": "admin"}, "old": {"kind": "server", "image": "old"}}, `+target+`}`)
	env, fa := newEnv(t, t.TempDir(), nil)
	fa.out["aws ecs describe-services"] = `{"services": [
		{"serviceName": "shop-web", "serviceArn": "arn:web", "status": "ACTIVE", "desiredCount": 2, "runningCount": 2, "deployments": [{"status": "PRIMARY", "rolloutState": "COMPLETED", "createdAt": "2026-10-08T01:00:00.123000+00:00", "taskDefinition": "arn:task:web:3"}]},
		{"serviceName": "shop-api", "serviceArn": "arn:api", "status": "ACTIVE", "desiredCount": 1, "runningCount": 0, "deployments": [{"status": "PRIMARY", "rolloutState": "FAILED", "rolloutStateReason": "tasks failed to start", "createdAt": "2026-10-08T02:00:00+00:00"}]},
		{"serviceName": "shop-old", "status": "INACTIVE"}]}`
	fa.out["aws ecs describe-task-definition --task-definition arn:task:web:3"] = `{"taskDefinition": {"containerDefinitions": [{"image": "123.dkr.ecr.us-east-1.amazonaws.com/apps/shop-web@sha256:cccc"}]}}`
	st, err := New().Status(context.Background(), s, env)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]adapter.ServiceStatus{}
	for _, ss := range st.Services {
		got[ss.Name] = ss
	}
	if web := got["web"]; web.State != "running" || web.Running != 2 || web.URL != "https://shop-web.ecs.us-east-1.on.aws" || web.Detail != "" ||
		web.Since != "2026-10-08T01:00:00Z" || web.Image != "123.dkr.ecr.us-east-1.amazonaws.com/apps/shop-web@sha256:cccc" || web.Restarts != nil || len(web.Events) != 0 {
		t.Errorf("web = %+v", web)
	}
	if api := got["api"]; api.State != "failing" || api.Detail != "tasks failed to start" || api.Since != "2026-10-08T02:00:00Z" || api.Image != "" || !slices.Equal(api.Events, []string{"tasks failed to start"}) {
		t.Errorf("api = %+v", api)
	}
	if got["admin"].State != "missing" || got["old"].State != "missing" {
		t.Errorf("admin = %+v, old = %+v", got["admin"], got["old"])
	}
}

func TestLogs(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}, "api": {"kind": "server", "image": "api"}}, `+target+`}`)
	env, fa := newEnv(t, t.TempDir(), nil)
	fa.out["aws ecs describe-services"] = `{"services": [{"serviceName": "shop-web", "serviceArn": "arn:web", "status": "ACTIVE"}]}`
	if err := New().Logs(context.Background(), s, env, adapter.LogOptions{Service: "web", Follow: true, Since: "10m"}); err != nil {
		t.Fatal(err)
	}
	if fa.find("aws logs tail /aws/ecs/default/shop-web --format short --log-stream-name-prefix ecs --since 10m --follow") == nil {
		t.Errorf("calls = %v", fa.calls)
	}
	if err := New().Logs(context.Background(), s, env, adapter.LogOptions{Follow: true}); err == nil {
		t.Error("following every service should be refused")
	}
	if err := New().Logs(context.Background(), s, env, adapter.LogOptions{Service: "api"}); err == nil || !strings.Contains(err.Error(), "isn't deployed") {
		t.Errorf("api err = %v", err)
	}
}

// A second destroy while the first is still draining must not say the
// deployment is gone: its load balancer is still billing.
func TestDestroyReportsDrainingServices(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {
		"web": {"kind": "server", "image": "nginx"}, "api": {"kind": "server", "image": "api"}}, `+target+`}`)
	for name, tc := range map[string]struct {
		services string
		deletes  int
		want     []string
		not      string
	}{
		"only draining": {
			services: `{"serviceName": "shop-web", "serviceArn": "arn:web", "status": "DRAINING"}`,
			want:     []string{"Still being deleted by an earlier destroy: shop-web."},
			not:      "Removed shop",
		},
		"draining and active": {
			services: `{"serviceName": "shop-web", "serviceArn": "arn:web", "status": "DRAINING"}, {"serviceName": "shop-api", "serviceArn": "arn:api", "status": "ACTIVE"}`,
			deletes:  1,
			want:     []string{"Removed shop from ECS", "Still being deleted by an earlier destroy: shop-web."},
		},
	} {
		t.Run(name, func(t *testing.T) {
			env, fa := newEnv(t, t.TempDir(), nil)
			fa.out["aws ecs describe-services"] = `{"services": [` + tc.services + `]}`
			res, err := New().Destroy(context.Background(), s, env, adapter.DestroyOptions{})
			if err != nil || !res.OK {
				t.Fatalf("destroy: %v %+v", err, res)
			}
			got := strings.Join(res.Messages, "\n")
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("messages %q are missing %q", got, want)
				}
			}
			if strings.Contains(got, "Nothing to remove") || tc.not != "" && strings.Contains(got, tc.not) {
				t.Errorf("messages %q claim more than happened", got)
			}
			if n := fa.count("aws ecs delete-express-gateway-service"); n != tc.deletes {
				t.Errorf("%d delete calls, want %d", n, tc.deletes)
			}
			if tc.deletes == 1 && fa.find("aws ecs delete-express-gateway-service --service-arn arn:api") == nil {
				t.Errorf("deleted the wrong service: %v", fa.calls)
			}
		})
	}
}

func TestDestroy(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {
		"web": {"kind": "server", "image": "nginx", "secrets": ["TOKEN"]}, "api": {"kind": "server", "image": "api"}},
		"secrets": {"TOKEN": {}}, `+target+`}`)
	env, fa := newEnv(t, t.TempDir(), nil)
	fa.out["aws ecs describe-services"] = `{"services": [{"serviceName": "shop-web", "serviceArn": "arn:web", "status": "ACTIVE"}]}`
	res, err := New().Destroy(context.Background(), s, env, adapter.DestroyOptions{Volumes: true})
	if err != nil || !res.OK {
		t.Fatalf("destroy: %v %+v", err, res)
	}
	if fa.find("aws ecs delete-express-gateway-service --service-arn arn:web") == nil || fa.count("aws ecs delete-express-gateway-service") != 1 {
		t.Errorf("deletes wrong services: %v", fa.calls)
	}
	if fa.find("aws secretsmanager delete-secret --secret-id anyship/shop/TOKEN --force-delete-without-recovery") == nil {
		t.Error("secret not deleted with --volumes")
	}
	summary, err := New().DestroySummary(s, adapter.DestroyOptions{})
	if err != nil || !strings.Contains(strings.Join(summary, "\n"), "Keep the Secrets Manager secrets anyship/shop/TOKEN") {
		t.Errorf("summary = %v %v", summary, err)
	}
}

// The spec is YAML: the hint for a missing target block names the path in
// anyship.yaml and shows fields as they are written there, never as JSON.
func TestMissingOptionsHint(t *testing.T) {
	s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "nginx"}}}`)
	env, _ := newEnv(t, t.TempDir(), nil)
	p := plan(t, s, env)
	if len(p.Findings) != 1 || p.Findings[0].Code != "AWS_BAD_OPTIONS" {
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
		"nginx":                  "latest",
		"ghcr.io/acme/shop:main": "main",
		"nginx:1.27":             "",
		"ghcr.io/acme/shop@sha256:0000000000000000000000000000000000000000000000000000000000000000": "",
	} {
		s := parse(t, `{"name": "shop", "services": {"web": {"kind": "server", "image": "`+image+`"}}, `+target+`}`)
		env, _ := newEnv(t, t.TempDir(), nil)
		p := plan(t, s, env)
		var warning *adapter.Finding
		for i, f := range p.Findings {
			if f.Code == "AWS_MUTABLE_TAG" {
				warning = &p.Findings[i]
			}
		}
		switch {
		case tag == "" && warning != nil:
			t.Errorf("%s: unexpected warning %q", image, warning.Message)
		case tag != "" && warning == nil:
			t.Errorf("%s: no AWS_MUTABLE_TAG warning in %v", image, codes(p, adapter.Warning))
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
