package diagnose

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

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

func TestApplyPatchKeepsOrderAndComments(t *testing.T) {
	got, err := ApplyPatch([]byte(specYAML), []PatchOp{
		{Op: "replace", Path: "/spec/services/web/ports/0/port", Value: "8080"},
		{Op: "add", Path: "/spec/services/web/env/PORT", Value: `"8080"`},
		{Op: "remove", Path: "/spec/services/web/env/MODE"},
		{Op: "add", Path: "/spec/services/web/ports/-", Value: `{"port": 9000, "protocol": "udp"}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `# yaml-language-server: $schema=https://example.com/schema.json
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
        - port: 8080
        - port: 9000
          protocol: udp
      env:
        PORT: "8080"
`
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyPatchRejectsBadOps(t *testing.T) {
	for _, op := range []PatchOp{
		{Op: "replace", Path: "/spec/services/api/image", Value: `"x"`},
		{Op: "remove", Path: "/spec/services/web/ports/3"},
		{Op: "replace", Path: "/spec/services/web/image", Value: `not json`},
		{Op: "replace", Path: "/spec/services/web/image", Value: `x: 1`},
		{Op: "move", Path: "/metadata/name", Value: `"x"`},
		{Op: "replace", Path: "", Value: `{}`},
		{Op: "replace", Path: "name", Value: `"x"`},
	} {
		if _, err := ApplyPatch([]byte(specYAML), []PatchOp{op}); err == nil {
			t.Errorf("%+v should fail", op)
		}
	}
}

func TestDescribePatch(t *testing.T) {
	got := DescribePatch([]byte(specYAML), []PatchOp{
		{Op: "replace", Path: "/spec/services/web/ports/0/port", Value: "8080"},
		{Op: "remove", Path: "/spec/services/web/env"},
		{Op: "add", Path: "/spec/services/web/start", Value: `"node server.js"`},
	})
	want := []string{
		"replace /spec/services/web/ports/0/port: 3000 → 8080",
		`remove /spec/services/web/env (was {"MODE":"prod"})`,
		`add /spec/services/web/start: "node server.js"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

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

// scriptedModel returns diagnoses in order and records the requests.
type scriptedModel struct {
	answers  []*Diagnosis
	requests []Request
}

func (m *scriptedModel) Diagnose(_ context.Context, req Request) (*Diagnosis, error) {
	m.requests = append(m.requests, req)
	d := m.answers[0]
	if len(m.answers) > 1 {
		m.answers = m.answers[1:]
	}
	return d, nil
}

func TestRunRetriesRejectedPatches(t *testing.T) {
	bad := &Diagnosis{Summary: "port", SpecPatch: []PatchOp{{Op: "replace", Path: "/spec/services/api/ports/0/port", Value: "8080"}}}
	good := &Diagnosis{Summary: "port", SpecPatch: []PatchOp{{Op: "replace", Path: "/spec/services/web/ports/0/port", Value: "8080"}}}
	model := &scriptedModel{answers: []*Diagnosis{bad, good}}
	validate := func(patched []byte) error {
		_, err := spec.Parse(patched)
		return err
	}

	r, err := Run(context.Background(), model, "ctx", []byte(specYAML), validate)
	if err != nil {
		t.Fatal(err)
	}
	if r.Patched == nil || !strings.Contains(string(r.Patched), "- port: 8080") {
		t.Errorf("result = %+v", r)
	}
	if len(model.requests) != 2 || len(model.requests[1].Rejected) != 1 || !strings.Contains(model.requests[1].Rejected[0].Problem, `"api" does not exist`) {
		t.Errorf("the rejection should be fed back: %+v", model.requests)
	}

	stubborn := &scriptedModel{answers: []*Diagnosis{bad}}
	r, err = Run(context.Background(), stubborn, "ctx", []byte(specYAML), validate)
	if err != nil {
		t.Fatal(err)
	}
	if r.Patched != nil || r.PatchProblem == "" || len(stubborn.requests) != MaxAttempts {
		t.Errorf("after %d rejected attempts: result = %+v, requests = %d", MaxAttempts, r, len(stubborn.requests))
	}

	noPatch := &scriptedModel{answers: []*Diagnosis{{Summary: "host is down"}}}
	r, _ = Run(context.Background(), noPatch, "ctx", []byte(specYAML), validate)
	if r.Patched != nil || r.PatchProblem != "" || r.Diagnosis.Summary != "host is down" {
		t.Errorf("result = %+v", r)
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
func fakeClaude(t *testing.T, status int, response string) (*Claude, *http.Request, *map[string]any) {
	t.Helper()
	var captured http.Request
	body := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = *r
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	return NewClaude("", "", option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0)), &captured, &body
}

func message(stopReason, text string) string {
	data, _ := json.Marshal(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": DefaultModel,
		"content":     []map[string]any{{"type": "text", "text": text}},
		"stop_reason": stopReason, "stop_sequence": nil,
		"stop_details": map[string]any{"type": "refusal", "category": "cyber", "explanation": "declined for testing"},
		"usage":        map[string]any{"input_tokens": 10, "output_tokens": 20},
	})
	return string(data)
}

func TestClaudeRequestAndResponse(t *testing.T) {
	answer, _ := json.Marshal(Diagnosis{
		Summary: "The app listens on 3000 but the spec publishes 8080.", RootCause: "port mismatch", Confidence: "high",
		Evidence: []string{"EADDRINUSE"}, Steps: []string{"fix the port"},
		SpecPatch: []PatchOp{{Op: "replace", Path: "/spec/services/web/ports/0/port", Value: "3000"}},
	})
	claude, req, body := fakeClaude(t, 200, message("end_turn", string(answer)))

	d, err := claude.Diagnose(context.Background(), Request{Context: "## Recent logs\n...", Rejected: []Attempt{{Patch: []PatchOp{{Op: "remove", Path: "/x"}}, Problem: `"x" does not exist`}}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Confidence != "high" || len(d.SpecPatch) != 1 || d.SpecPatch[0].Value != "3000" {
		t.Errorf("diagnosis = %+v", d)
	}

	if req.URL.Path != "/v1/messages" || !strings.Contains(req.Header.Get("anthropic-beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("request = %s %s, beta header %q", req.Method, req.URL.Path, req.Header.Get("anthropic-beta"))
	}
	b := *body
	if b["model"] != DefaultModel || b["fallbacks"] != "default" {
		t.Errorf("model/fallbacks = %v / %v", b["model"], b["fallbacks"])
	}
	output := b["output_config"].(map[string]any)
	format := output["format"].(map[string]any)
	if output["effort"] != DefaultEffort || format["type"] != "json_schema" || format["schema"].(map[string]any)["additionalProperties"] != false {
		t.Errorf("output_config = %v", output)
	}
	system := b["system"].([]any)[0].(map[string]any)
	if system["cache_control"].(map[string]any)["type"] != "ephemeral" || !strings.Contains(system["text"].(string), `"$schema"`) {
		t.Error("the system prompt should carry the anyship.yaml schema and be cached")
	}
	user := b["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(user, "## Recent logs") || !strings.Contains(user, "rejected") {
		t.Errorf("user prompt = %q", user)
	}
	if _, ok := b["thinking"]; ok {
		t.Error("thinking should be left to the model's default (adaptive)")
	}
}

func TestClaudeOmitsFallbacksForOtherModels(t *testing.T) {
	claude, req, body := fakeClaude(t, 200, message("end_turn", `{"summary":"s","root_cause":"r","confidence":"low","evidence":[],"steps":[],"spec_patch":[],"code_change":""}`))
	claude.model = "claude-haiku-4-5"
	if _, err := claude.Diagnose(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := (*body)["fallbacks"]; ok || strings.Contains(req.Header.Get("anthropic-beta"), "fallback") {
		t.Error("fallbacks should only be sent to models that accept them")
	}
}

func TestClaudeErrors(t *testing.T) {
	refused, _, _ := fakeClaude(t, 200, message("refusal", ""))
	if _, err := refused.Diagnose(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "declined to answer (cyber)") {
		t.Errorf("refusal: %v", err)
	}
	unauthorized, _, _ := fakeClaude(t, 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	if _, err := unauthorized.Diagnose(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("401: %v", err)
	}
	garbled, _, _ := fakeClaude(t, 200, message("end_turn", "not json"))
	if _, err := garbled.Diagnose(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "expected JSON") {
		t.Errorf("bad JSON: %v", err)
	}
}
