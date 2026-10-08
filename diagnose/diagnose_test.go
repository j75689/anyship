package diagnose

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/internal/spectest"
	"github.com/j75689/anyship/spec"
)

const specYAML = `# yaml-language-server: $schema=https://example.com/schema.json
apiVersion: anyship/v1alpha1
kind: App
metadata:
  name: shop
spec:
  services:
    web:
      kind: server
      image: shop:1 # pinned for the launch
      ports:
        - port: 3000
      env:
        MODE: prod
`

func TestRedactor(t *testing.T) {
	s, err := spectest.Parse(`{"name": "app", "services": {"web": {"kind": "server", "image": "x", "secrets": ["DB_PASSWORD"]}}, "secrets": {"DB_PASSWORD": {}}}`)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRedactor(s, func(name string) (string, bool) {
		if name == "DB_PASSWORD" {
			return "hunter2-very-secret", true
		}
		return "", false
	})
	in := strings.Join([]string{
		"connecting with hunter2-very-secret",
		`"API_KEY": "abc123"`,
		"STRIPE_SECRET=sk_live_123",
		"GITHUB_TOKEN: ghs_yaml_value",
		"postgres://admin:s3cret@db:5432/shop",
		"auth sk-ant-api03-abcdefghijklmnopqrstuvwxyz",
		"AKIAABCDEFGHIJKLMNOP",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----",
		"port 8080 is fine",
	}, "\n")
	got := r.Redact(in)
	for _, leaked := range []string{"hunter2", "abc123", "sk_live_123", "ghs_yaml_value", "s3cret", "sk-ant-api03", "AKIAABCDEFGHIJKLMNOP", "OPENSSH PRIVATE KEY"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q leaked:\n%s", leaked, got)
		}
	}
	for _, kept := range []string{"[redacted secret DB_PASSWORD]", "postgres://admin:[redacted]@db:5432/shop", "port 8080 is fine"} {
		if !strings.Contains(got, kept) {
			t.Errorf("missing %q:\n%s", kept, got)
		}
	}
}

// Shapes that used to reach Claude in clear text.
func TestRedactorCoversWholeValues(t *testing.T) {
	r := NewRedactor(&spec.Spec{}, func(string) (string, bool) { return "", false })
	for _, tc := range []struct{ name, in, want string }{
		{"url password with an empty username", "REDIS_URL: redis://:zzLEAK@cache:6379/0", "REDIS_URL: redis://:[redacted]@cache:6379/0"},
		{"url password containing @", "amqp://app:zz@LEAK@mq:5672", "amqp://app:[redacted]@mq:5672"},
		{"unquoted value with spaces", "TOKEN_ARGS: --api-token zzLEAK", `TOKEN_ARGS: "[redacted]"`},
		{"env line with spaces", "      - API_TOKEN=zz LEAK", `      - API_TOKEN="[redacted]"`},
		{"exported variable with spaces", "export DB_PASSWORD=zz LEAK", `export DB_PASSWORD="[redacted]"`},
		{"compose log line", "web-1  | X-Auth-Token: Bearer zzLEAK", `web-1  | X-Auth-Token: "[redacted]"`},
		{"json number keeps its comma", `  "maxTokens": 4096,`, `  "maxTokens": "[redacted]",`},
		{"block scalar", "    TLS_KEY: |\n      zzLEAK one\n\n      zzLEAK two\n    PORT: 80", "    TLS_KEY: |\n      [redacted]\n    PORT: 80"},
		{"folded block scalar in a list", "- PRIVATE_NOTE: >-\n    zzLEAK\n- name: web", "- PRIVATE_NOTE: >-\n    [redacted]\n- name: web"},
		{"authorization header", "Authorization: Bearer zzLEAK", "Authorization: Bearer [redacted]"},
		{"basic authorization in curl", `curl -H "Authorization: Basic enpMRUFLOnp6" https://api.example.com`, `curl -H "Authorization: Basic [redacted]" https://api.example.com`},
		{"authorization in json", `{"authorization": "Bearer zzLEAK", "port": 80}`, `{"authorization": "Bearer [redacted]", "port": 80}`},
		{"authorization without a scheme", "proxy-authorization=zzLEAK", "proxy-authorization=[redacted]"},
		// Mid-line pairs keep their neighbours: only the value goes.
		{"libpq string", "dsn host=db password=zzLEAK dbname=shop", `dsn host=db password="[redacted]" dbname=shop`},
	} {
		got := r.Redact(tc.in)
		if got != tc.want {
			t.Errorf("%s: Redact(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		if strings.Contains(got, "LEAK") {
			t.Errorf("%s: leaked: %q", tc.name, got)
		}
		if again := r.Redact(got); again != got {
			t.Errorf("%s: redacting twice changed %q to %q", tc.name, got, again)
		}
	}
}

func TestRedactorKeepsStructure(t *testing.T) {
	s, _ := spec.Parse([]byte(specYAML))
	r := NewRedactor(s, func(name string) (string, bool) { return "", false })
	// Keys that look secret but hold arrays or objects are spec structure, and
	// redacted values must not be redacted again.
	for _, in := range []string{
		`"secrets": ["JWT_SECRET"],`,
		`"secrets": {`,
		`"JWT_SECRET": { "generate": "hex32" }`,
		`JWT_SECRET=[redacted secret JWT_SECRET] is set`,
		`"API_KEY": ""`,
		"  secrets:\n    JWT_SECRET:\n      generate: hex32",
		"      secrets:\n        - JWT_SECRET",
		"JWT_SECRET: {generate: hex32}",
		"    authorization:\n      type: bearer",
		"Authorization: Bearer [redacted token]",
		"description: |\n  not a secret\nport: 80",
		"      private: false        # true: public services require authentication",
		`{"private": true, "region": "us-central1"}`,
		"API_KEY: |\nport: 80",
	} {
		if got := r.Redact(in); got != in {
			t.Errorf("Redact(%q) = %q, want it unchanged", in, got)
		}
	}
}

// fakeTarget reports a little of everything, including a leaked token.
type fakeTarget struct{}

func (fakeTarget) Name() string        { return "fake" }
func (fakeTarget) Description() string { return "" }
func (fakeTarget) Plan(_ context.Context, _ *spec.Spec, env *adapter.Env) (*adapter.Plan, error) {
	return &adapter.Plan{
		Target:   "fake",
		Findings: []adapter.Finding{{Level: adapter.Info, Code: "FAKE_NOTE", Message: "noted"}},
		Files:    []adapter.File{{Path: "/tmp/.anyship/fake/compose.yaml", Contents: []byte(`{"environment": {"API_TOKEN": "tok-123"}}`)}},
	}, nil
}
func (fakeTarget) Apply(_ context.Context, _ *adapter.Plan, _ *spec.Spec, env *adapter.Env) (*adapter.Result, error) {
	env.Logf("dry run=%v", env.DryRun)
	return &adapter.Result{OK: false, Findings: []adapter.Finding{{Level: adapter.Error, Code: "FAKE_PORT_IN_USE", Message: "port 3000 is taken"}}}, nil
}
func (fakeTarget) Status(context.Context, *spec.Spec, *adapter.Env) (*adapter.Status, error) {
	return &adapter.Status{Target: "fake", Deployed: true, Services: []adapter.ServiceStatus{{Name: "web", State: "restarting", Desired: 1}}}, nil
}
func (fakeTarget) Logs(_ context.Context, _ *spec.Spec, env *adapter.Env, opts adapter.LogOptions) error {
	env.Logf("Error: listen EADDRINUSE :::3000 (tail=%d)", opts.Tail)
	env.Logf("using key sk-ant-api03-abcdefghijklmnopqrstuvwxyz")
	return nil
}

func TestCollectGathersAndRedacts(t *testing.T) {
	s, err := spec.Parse([]byte(specYAML))
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	c := Collect(context.Background(), Inputs{
		Spec: s, SpecPath: "/p/anyship.yaml", SpecRaw: []byte(specYAML), Adapter: fakeTarget{},
		NewEnv: func(dryRun bool, out io.Writer) *adapter.Env {
			return &adapter.Env{DryRun: dryRun, Logf: func(f string, a ...any) { _, _ = fmt.Fprintf(out, f+"\n", a...) }}
		},
		Note:     "it keeps restarting",
		Checks:   true,
		Progress: func(step string) { steps = append(steps, step) },
		Redactor: NewRedactor(s, func(string) (string, bool) { return "", false }),
	})
	text := c.Render()
	for _, want := range []string{
		"## anyship.yaml (anyship.yaml), deploying to the fake target",
		"## What the user reports", "it keeps restarting",
		`"code": "FAKE_NOTE"`,
		"## Generated file compose.yaml",
		"## Target checks (dry run)", "FAKE_PORT_IN_USE", "dry run=true",
		`"state": "restarting"`,
		"EADDRINUSE :::3000 (tail=200)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("context is missing %q:\n%s", want, text)
		}
	}
	for _, leaked := range []string{"tok-123", "sk-ant-api03"} {
		if strings.Contains(text, leaked) {
			t.Errorf("%q leaked into the context", leaked)
		}
	}
	if len(steps) != 4 {
		t.Errorf("progress steps = %q", steps)
	}
}

// fakeClaude serves canned Messages API responses and records the request.
