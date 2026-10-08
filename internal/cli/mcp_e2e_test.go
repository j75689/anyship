package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

// TestMCPEndToEnd drives the built binary's MCP server over stdio through a
// whole deploy of the Flask sample to the kubernetes target, the way an
// agent would: detect, validate, plan, dry run, apply, status, logs,
// diagnose, destroy. It runs when ANYSHIP_MCP_E2E=1, with ANYSHIP naming
// the binary, against the current kubectl context (or ANYSHIP_K8S_CONTEXT)
// and a registry the nodes pull from (ANYSHIP_K8S_REPOSITORY). What it
// checks is what the in-memory tests can't: that a real target's output
// reaches the tool result and nothing else reaches the protocol.
func TestMCPEndToEnd(t *testing.T) {
	if os.Getenv("ANYSHIP_MCP_E2E") != "1" {
		t.Skip("set ANYSHIP_MCP_E2E=1, ANYSHIP and a kubectl context to run")
	}
	bin, err := filepath.Abs(os.Getenv("ANYSHIP"))
	if err != nil || os.Getenv("ANYSHIP") == "" {
		t.Fatal("ANYSHIP must name the anyship binary")
	}
	cluster := os.Getenv("ANYSHIP_K8S_CONTEXT")
	if cluster == "" {
		out, err := exec.Command("kubectl", "config", "current-context").Output()
		if err != nil {
			t.Fatalf("no kubectl context: %v", err)
		}
		cluster = strings.TrimSpace(string(out))
	}
	repository := os.Getenv("ANYSHIP_K8S_REPOSITORY")
	if repository == "" {
		repository = "localhost:5000/anyship-e2e"
	}
	const namespace = "anyship-mcp-e2e"
	kubectl := func(args ...string) (string, error) {
		out, err := exec.Command("kubectl", append([]string{"--context", cluster, "-n", namespace}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := exec.Command("kubectl", "--context", cluster, "create", "namespace", namespace).CombinedOutput(); err != nil {
		t.Fatalf("creating namespace %s: %v\n%s", namespace, err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context", cluster, "delete", "namespace", namespace, "--ignore-not-found", "--wait=false").Run()
	})

	// The sample app, in a directory of its own: the server runs there, so
	// config and dir default to it, as for an agent started in a project.
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("..", "..", "dockerfile", "testdata", "apps", "python-flask"))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	connect := func(args ...string) *mcp.ClientSession {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"mcp"}, args...)...)
		cmd.Dir = dir
		cmd.Stderr = os.Stderr
		session, err := mcp.NewClient(&mcp.Implementation{Name: "e2e"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
		if err != nil {
			t.Fatalf("connecting to %s mcp %s: %v", bin, strings.Join(args, " "), err)
		}
		t.Cleanup(func() { _ = session.Close() })
		return session
	}
	call := func(session *mcp.ClientSession, tool string, args map[string]any, out any) string {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: protocol error: %v", tool, err)
		}
		if res.IsError {
			var texts []string
			for _, c := range res.Content {
				if text, ok := c.(*mcp.TextContent); ok {
					texts = append(texts, text.Text)
				}
			}
			return strings.Join(texts, "\n")
		}
		if out != nil {
			data, err := json.Marshal(res.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, out); err != nil {
				t.Fatalf("%s: %v\n%s", tool, err, data)
			}
		}
		return ""
	}
	mustCall := func(session *mcp.ClientSession, tool string, args map[string]any, out any) {
		t.Helper()
		if msg := call(session, tool, args, out); msg != "" {
			t.Fatalf("%s failed: %s", tool, msg)
		}
	}
	deployer := connect("--allow-deploy")

	t.Log("== the target's page and the schema are resources")
	var targets targetsOutput
	mustCall(deployer, "targets", nil, &targets)
	if !slices.ContainsFunc(targets.Targets, func(ti targetInfo) bool { return ti.Name == "kubernetes" && ti.Docs == "anyship://targets/kubernetes" }) {
		t.Errorf("targets = %+v", targets.Targets)
	}
	page, err := deployer.ReadResource(ctx, &mcp.ReadResourceParams{URI: "anyship://targets/kubernetes"})
	if err != nil || len(page.Contents) != 1 || !strings.Contains(page.Contents[0].Text, "ingressClass") {
		t.Errorf("kubernetes page: %v %+v", err, page)
	}
	schema, err := deployer.ReadResource(ctx, &mcp.ReadResourceParams{URI: "anyship://schema"})
	if err != nil || len(schema.Contents) != 1 || !strings.Contains(schema.Contents[0].Text, `"$schema"`) {
		t.Errorf("schema: %v", err)
	}

	t.Log("== detect drafts the spec, which is written with the kubernetes target")
	var detected detectOutput
	mustCall(deployer, "detect", nil, &detected)
	if !detected.Valid || !strings.Contains(detected.Spec, "flask") {
		t.Fatalf("detect = %+v", detected)
	}
	draft := detected.Spec + "  targets:\n    kubernetes:\n      context: " + cluster + "\n      namespace: " + namespace + "\n      repository: " + repository + "\n"
	if err := os.WriteFile(filepath.Join(dir, spec.Filename), []byte(draft), 0o644); err != nil {
		t.Fatal(err)
	}
	var valid validateOutput
	mustCall(deployer, "validate", nil, &valid)
	if !valid.Valid || !slices.Equal(valid.Services, []string{"web"}) || !slices.Equal(valid.Targets, []string{"kubernetes"}) {
		t.Fatalf("validate = %+v", valid)
	}
	mustCall(deployer, "detect", nil, &detected)
	if ex := detected.Existing; ex == nil || !ex.Valid || !slices.Equal(ex.Targets, []string{"kubernetes"}) {
		t.Errorf("detect should now report the spec in place: %+v", detected.Existing)
	}

	t.Log("== plan brings the generated files with their contents and writes nothing")
	var planned planOutput
	mustCall(deployer, "plan", map[string]any{"target": "kubernetes"}, &planned)
	if !planned.Ready {
		t.Fatalf("plan not ready: %+v", planned.Findings)
	}
	var names []string
	for _, f := range planned.Files {
		names = append(names, filepath.Base(f.Path))
		if f.Contents == "" {
			t.Errorf("plan file %s has no contents", f.Path)
		}
	}
	if !slices.Contains(names, "manifests.yaml") || !slices.Contains(names, "web.Dockerfile") {
		t.Errorf("plan files = %v", names)
	}
	if _, err := os.Stat(filepath.Join(dir, ".anyship")); err == nil {
		t.Error("plan must not write .anyship/")
	}
	for _, f := range planned.Findings {
		if strings.Contains(f.Hint, "anyship apply") || strings.Contains(f.Message, "anyship apply") {
			t.Errorf("finding worded for a shell, not an agent: %+v", f)
		}
	}

	t.Log("== apply with dry_run runs the checks, writes the files, deploys nothing")
	var dry resultOutput
	mustCall(deployer, "apply", map[string]any{"target": "kubernetes", "dry_run": true}, &dry)
	if !dry.OK || !slices.ContainsFunc(dry.Findings, func(f adapter.Finding) bool { return f.Code == "K8S_PREFLIGHT_OK" }) {
		t.Fatalf("dry run = %+v", dry)
	}
	if _, err := os.Stat(filepath.Join(dir, ".anyship", "kubernetes", "manifests.yaml")); err != nil {
		t.Error("a dry run should write the manifests under .anyship/kubernetes/")
	}
	if out, _ := kubectl("get", "deployments", "-o", "name"); out != "" {
		t.Errorf("a dry run deployed something: %s", out)
	}

	t.Log("== apply deploys; the build's and kubectl's output comes back in the result")
	var applied resultOutput
	mustCall(deployer, "apply", map[string]any{"target": "kubernetes"}, &applied)
	if !applied.OK {
		t.Fatalf("apply = %+v", applied)
	}
	for _, want := range []string{"docker buildx build", "deployment.apps/app-web", "successfully rolled out"} {
		if !strings.Contains(applied.Output, want) {
			t.Errorf("apply output lacks %q (truncated=%v):\n%s", want, applied.Truncated, applied.Output)
		}
	}
	if len(applied.Output) > maxToolOutput {
		t.Errorf("apply output is %d bytes, over the %d cap", len(applied.Output), maxToolOutput)
	}
	if !slices.Contains(applied.Messages, "web: http://app-web."+namespace+".svc:8000 (inside the cluster)") {
		t.Errorf("apply messages = %v", applied.Messages)
	}
	if !slices.ContainsFunc(applied.Files, func(f string) bool { return filepath.Base(f) == "manifests.yaml" }) {
		t.Errorf("apply should name the files it wrote: %v", applied.Files)
	}

	t.Log("== status and logs read the cluster")
	var st statusOutput
	mustCall(deployer, "status", map[string]any{"target": "kubernetes"}, &st)
	if !st.Deployed || !st.Healthy || st.Location != cluster+"/"+namespace {
		t.Errorf("status = %+v", st)
	}
	// Without a readiness probe the rollout is done the moment the container
	// runs, which can be before the server has printed anything.
	var logs logsOutput
	for try := 0; try < 10; try++ {
		started := time.Now()
		mustCall(deployer, "logs", map[string]any{"target": "kubernetes", "tail": 20, "timestamps": true}, &logs)
		if took := time.Since(started); took > liveLogWindow {
			t.Errorf("logs took %s; a target that reads recent logs should return at once", took)
		}
		if strings.Contains(strings.ToLower(logs.Output), "gunicorn") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !strings.Contains(strings.ToLower(logs.Output), "gunicorn") || logs.Note != "" || !strings.Contains(logs.Output, "2026-") {
		t.Errorf("logs = %+v", logs)
	}

	t.Log("== diagnose_context collects everything and leaves .anyship/ alone")
	before, _ := os.ReadDir(filepath.Join(dir, ".anyship", "kubernetes"))
	var diagnosed diagnoseContextOutput
	mustCall(deployer, "diagnose_context", map[string]any{"target": "kubernetes", "note": "just checking"}, &diagnosed)
	for _, want := range []string{"app-web", "K8S_PREFLIGHT_OK", "just checking"} {
		if !strings.Contains(diagnosed.Context, want) {
			t.Errorf("diagnose context lacks %q", want)
		}
	}
	after, _ := os.ReadDir(filepath.Join(dir, ".anyship", "kubernetes"))
	if len(after) != len(before) {
		t.Errorf("diagnose_context changed .anyship/kubernetes/: %d entries, was %d", len(after), len(before))
	}

	t.Log("== a server without --allow-deploy only takes dry runs")
	reader := connect()
	if msg := call(reader, "apply", map[string]any{"target": "kubernetes"}, nil); !strings.Contains(msg, "--allow-deploy") {
		t.Errorf("apply without --allow-deploy: %q", msg)
	}
	var readerDry resultOutput
	mustCall(reader, "apply", map[string]any{"target": "kubernetes", "dry_run": true}, &readerDry)
	if !readerDry.OK {
		t.Errorf("dry run without --allow-deploy = %+v", readerDry)
	}
	if msg := call(reader, "destroy", map[string]any{"target": "kubernetes"}, nil); !strings.Contains(msg, "--allow-deploy") {
		t.Errorf("destroy without --allow-deploy: %q", msg)
	}

	t.Log("== destroy: dry run, data deletion needs the confirmed name, then for real")
	var summary resultOutput
	mustCall(deployer, "destroy", map[string]any{"target": "kubernetes", "dry_run": true}, &summary)
	if !summary.OK || !slices.ContainsFunc(summary.Summary, func(s string) bool { return strings.Contains(s, "app-web") }) {
		t.Errorf("destroy dry run = %+v", summary)
	}
	if msg := call(deployer, "destroy", map[string]any{"target": "kubernetes", "volumes": true}, nil); !strings.Contains(msg, "confirm_project") {
		t.Errorf("destroy with volumes and no confirmation: %q", msg)
	}
	if out, _ := kubectl("get", "deployments", "-o", "name"); !strings.Contains(out, "app-web") {
		t.Error("a refused destroy must remove nothing")
	}
	var destroyed resultOutput
	mustCall(deployer, "destroy", map[string]any{"target": "kubernetes", "volumes": true, "confirm_project": "app"}, &destroyed)
	if !destroyed.OK || !strings.Contains(destroyed.Output, `deployment.apps "app-web" deleted`) {
		t.Errorf("destroy = %+v", destroyed)
	}
	mustCall(deployer, "status", map[string]any{"target": "kubernetes"}, &st)
	if st.Deployed || st.Healthy {
		t.Errorf("status after destroy = %+v", st)
	}
}
