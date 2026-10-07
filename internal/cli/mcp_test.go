package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/adapters/cloudflare"
	"github.com/j75689/anyship/adapters/vps"
	"github.com/j75689/anyship/spec"
)

// fakeAdapter runs real commands through env.Exec so tests can check that
// their output is captured instead of reaching the MCP transport.
type fakeAdapter struct{ destroyed *adapter.DestroyOptions }

func (*fakeAdapter) Name() string        { return "fake" }
func (*fakeAdapter) Description() string { return "test target" }

func (*fakeAdapter) Plan(_ context.Context, _ *spec.Spec, env *adapter.Env) (*adapter.Plan, error) {
	return &adapter.Plan{
		Target:  "fake",
		Actions: []adapter.Action{{Op: adapter.OpDeploy, Kind: "thing", Name: "x"}},
		Files:   []adapter.File{{Path: filepath.Join(env.OutDir, "generated.yaml"), Contents: []byte("generated: for review\n")}},
	}, nil
}

func (*fakeAdapter) Status(context.Context, *spec.Spec, *adapter.Env) (*adapter.Status, error) {
	return &adapter.Status{Target: "fake", Location: "nowhere", Deployed: false, Services: []adapter.ServiceStatus{}}, nil
}

// Apply writes the plan's files before it touches the target, dry run or not,
// like every real adapter; TestMCPReadOnlyToolsWriteNothing depends on it.
func (*fakeAdapter) Apply(ctx context.Context, p *adapter.Plan, _ *spec.Spec, env *adapter.Env) (*adapter.Result, error) {
	for _, f := range p.Files {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(f.Path, f.Contents, 0o644); err != nil {
			return nil, err
		}
		// Like the real targets, say where each file went.
		rel, _ := filepath.Rel(env.Dir, f.Path)
		env.Logf("wrote %s", filepath.ToSlash(rel))
	}
	env.Logf("$ deploying")
	execOpts, name, args := helper("streams")
	if err := env.Exec(ctx, execOpts, name, args...); err != nil {
		return &adapter.Result{Messages: []string{err.Error()}}, nil
	}
	return &adapter.Result{OK: true, Messages: []string{"deployed"}}, nil
}

func (*fakeAdapter) Logs(ctx context.Context, _ *spec.Spec, env *adapter.Env, opts adapter.LogOptions) error {
	execOpts, name, args := helper("print", "log line for "+opts.Service)
	return env.Exec(ctx, execOpts, name, args...)
}

func (f *fakeAdapter) DestroySummary(*spec.Spec, adapter.DestroyOptions) ([]string, error) {
	return []string{"remove x"}, nil
}

func (f *fakeAdapter) Destroy(_ context.Context, _ *spec.Spec, _ *adapter.Env, opts adapter.DestroyOptions) (*adapter.Result, error) {
	f.destroyed = &opts
	return &adapter.Result{OK: true, Messages: []string{"removed"}}, nil
}

type mcpHarness struct {
	session *mcp.ClientSession
	fake    *fakeAdapter
}

func connectMCP(t *testing.T, allowDeploy bool) *mcpHarness {
	t.Helper()
	fake := &fakeAdapter{}
	registry, err := adapter.NewRegistry(cloudflare.New(), vps.New(), fake)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{registry: registry, out: io.Discard, style: styler{}}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := newMCPServer(a, "test", allowDeploy).Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return &mcpHarness{session: session, fake: fake}
}

// call invokes a tool and decodes its structured output into out (if not nil).
// It returns the text of an error result, or "" on success.
func (h *mcpHarness) call(t *testing.T, tool string, args map[string]any, out any) string {
	t.Helper()
	res, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
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

func example(name string) string {
	return filepath.Join("..", "..", "examples", name, spec.Filename)
}

// fakeSpec writes a valid spec for the fake target and returns its path.
func fakeSpec(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), spec.Filename)
	if err := os.WriteFile(path, []byte("apiVersion: anyship/v1alpha1\nkind: App\nmetadata:\n  name: demo\nspec:\n  services:\n    web:\n      kind: server\n      image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMCPListsToolsWithSafetyHints(t *testing.T) {
	h := connectMCP(t, false)
	res, err := h.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	for _, name := range []string{"targets", "detect", "validate", "plan", "status", "logs", "diagnose_context"} {
		if tools[name] == nil || tools[name].Annotations == nil || !tools[name].Annotations.ReadOnlyHint {
			t.Errorf("%s should be listed as read-only", name)
			continue
		}
		// Reading twice is the same as reading once, and the hint is sent
		// either way: unset, it tells the host the opposite.
		if !tools[name].Annotations.IdempotentHint {
			t.Errorf("%s is read-only, so it should be listed as idempotent", name)
		}
	}
	if tools["apply"].Annotations.IdempotentHint {
		t.Error("apply deploys again each time; it is not idempotent")
	}
	for _, name := range []string{"apply", "destroy"} {
		if tools[name] == nil || tools[name].Annotations.ReadOnlyHint || tools[name].Annotations.DestructiveHint == nil || !*tools[name].Annotations.DestructiveHint {
			t.Errorf("%s should be listed as destructive", name)
		}
	}
}

func TestMCPReadOnlyTools(t *testing.T) {
	h := connectMCP(t, false)

	var targets targetsOutput
	h.call(t, "targets", nil, &targets)
	if len(targets.Targets) != 3 || targets.Targets[2].Name != "vps" || !slices.Contains(targets.Targets[2].Capabilities, "status") {
		t.Errorf("targets = %+v", targets)
	}
	if targets.Targets[2].Docs != "anyship://targets/vps" || targets.Targets[1].Docs != "" {
		t.Errorf("targets should name the page of the targets that have one: %+v", targets.Targets)
	}

	var detected detectOutput
	if msg := h.call(t, "detect", map[string]any{"dir": filepath.Join("..", "..", "dockerfile", "testdata", "apps", "go")}, &detected); msg != "" {
		t.Fatal(msg)
	}
	if !detected.Valid || !strings.Contains(detected.Spec, "name: goapp") {
		t.Errorf("detect = %+v", detected)
	}

	var valid validateOutput
	h.call(t, "validate", map[string]any{"config": example("ethereum-node")}, &valid)
	if !valid.Valid || !slices.Equal(valid.Services, []string{"lighthouse", "reth"}) {
		t.Errorf("validate = %+v", valid)
	}
	var missing validateOutput
	h.call(t, "validate", map[string]any{"config": "does-not-exist.json"}, &missing)
	if missing.Valid || len(missing.Problems) == 0 {
		t.Errorf("validate of a missing file = %+v", missing)
	}

	var ready, refused planOutput
	h.call(t, "plan", map[string]any{"config": example("ethereum-node"), "target": "vps"}, &ready)
	h.call(t, "plan", map[string]any{"config": example("ethereum-node"), "target": "cloudflare"}, &refused)
	if !ready.Ready || len(ready.Files) != 1 || !strings.Contains(ready.Files[0].Contents, "reth") {
		t.Errorf("vps plan = %+v", ready)
	}
	if refused.Ready || !slices.ContainsFunc(refused.Findings, func(f adapter.Finding) bool { return f.Code == "CF_VOLUMES" }) {
		t.Errorf("cloudflare plan = %+v", refused)
	}

	// The input schema marks target as required, so the SDK rejects the call.
	if msg := h.call(t, "plan", map[string]any{"config": example("ethereum-node")}, nil); !strings.Contains(msg, `missing properties: ["target"]`) {
		t.Errorf("plan without a target: %q", msg)
	}

	var logs logsOutput
	if msg := h.call(t, "logs", map[string]any{"config": fakeSpec(t), "target": "fake", "service": "web"}, &logs); msg != "" {
		t.Fatal(msg)
	}
	if logs.Output != "log line for web\n" {
		t.Errorf("logs = %+v", logs)
	}
}

func TestMCPOnlyDryRunsWithoutAllowDeploy(t *testing.T) {
	h := connectMCP(t, false)
	config := fakeSpec(t)
	for _, tool := range []string{"apply", "destroy"} {
		if msg := h.call(t, tool, map[string]any{"config": config, "target": "fake"}, nil); !strings.Contains(msg, "--allow-deploy") {
			t.Errorf("%s without --allow-deploy: %q", tool, msg)
		}
	}
	var dry resultOutput
	if msg := h.call(t, "destroy", map[string]any{"config": config, "target": "fake", "dry_run": true}, &dry); msg != "" || !dry.OK || dry.Summary[0] != "remove x" {
		t.Errorf("destroy dry run = %+v, %q", dry, msg)
	}
	if h.fake.destroyed != nil {
		t.Error("a dry run must not destroy anything")
	}
}

func TestMCPApplyCapturesCommandOutput(t *testing.T) {
	h := connectMCP(t, true)

	// Anything written to the real stdout would corrupt the stdio transport.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	var result resultOutput
	msg := h.call(t, "apply", map[string]any{"config": fakeSpec(t), "target": "fake"}, &result)
	os.Stdout = stdout
	_ = w.Close()
	leaked, _ := io.ReadAll(r)

	if msg != "" || !result.OK || !slices.Equal(result.Messages, []string{"deployed"}) {
		t.Fatalf("apply = %+v, %q", result, msg)
	}
	for _, want := range []string{"$ deploying\n", "from-stdout\n", "from-stderr\n"} {
		if !strings.Contains(result.Output, want) {
			t.Errorf("output %q is missing %q", result.Output, want)
		}
	}
	if len(leaked) > 0 {
		t.Errorf("wrote to stdout: %q", leaked)
	}
}

func TestMCPDestroyNeedsConfirmationToDeleteData(t *testing.T) {
	h := connectMCP(t, true)
	config := fakeSpec(t)
	if msg := h.call(t, "destroy", map[string]any{"config": config, "target": "fake", "volumes": true}, nil); !strings.Contains(msg, `confirm_project set to "demo"`) {
		t.Errorf("destroy volumes without confirmation: %q", msg)
	}
	if h.fake.destroyed != nil {
		t.Fatal("nothing should be destroyed without confirmation")
	}
	var result resultOutput
	h.call(t, "destroy", map[string]any{"config": config, "target": "fake", "volumes": true, "confirm_project": "demo"}, &result)
	if !result.OK || h.fake.destroyed == nil || !h.fake.destroyed.Volumes {
		t.Errorf("destroy = %+v, destroyed = %+v", result, h.fake.destroyed)
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	b := &tailBuffer{max: 5}
	_, _ = b.Write([]byte("abc"))
	_, _ = b.Write([]byte("defg"))
	if got, truncated := b.String(); got != "cdefg" || !truncated {
		t.Errorf("got %q, truncated=%v", got, truncated)
	}
}

// TestMCPOverStdio runs the real binary as an MCP server, the way an agent
// host does, and checks the protocol works end to end over stdin/stdout.
func TestMCPOverStdio(t *testing.T) {
	// go build -o uses the name as given, and Windows only runs a file that
	// carries an executable extension.
	bin := filepath.Join(t.TempDir(), "anyship")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "../../cmd/anyship")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "mcp")
	cmd.Dir = filepath.Join("..", "..", "examples", "ethereum-node")
	ctx := context.Background()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()

	if session.InitializeResult().ServerInfo.Name != "anyship" {
		t.Errorf("server info = %+v", session.InitializeResult().ServerInfo)
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "plan", Arguments: map[string]any{"target": "vps"}})
	if err != nil || res.IsError {
		t.Fatalf("plan: %v %+v", err, res)
	}
	data, _ := json.Marshal(res.StructuredContent)
	var plan planOutput
	if err := json.Unmarshal(data, &plan); err != nil || !plan.Ready || plan.Target != "vps" {
		t.Errorf("plan over stdio = %s (%v)", data, err)
	}
}

func TestMCPDiagnoseContext(t *testing.T) {
	h := connectMCP(t, false)
	var out diagnoseContextOutput
	if msg := h.call(t, "diagnose_context", map[string]any{"config": fakeSpec(t), "target": "fake", "note": "502s"}, &out); msg != "" {
		t.Fatal(msg)
	}
	for _, want := range []string{"## anyship.yaml", "502s", "## Generated file generated.yaml", "## Target checks (dry run)", "log line for"} {
		if !strings.Contains(out.Context, want) {
			t.Errorf("context is missing %q:\n%s", want, out.Context)
		}
	}
	// The checks ran in a temporary directory that no longer exists; the
	// context must not send the agent looking for files there.
	for _, gone := range []string{"anyship-diagnose", "wrote "} {
		if strings.Contains(out.Context, gone) {
			t.Errorf("context mentions %q:\n%s", gone, out.Context)
		}
	}
}

// TestMCPPlanReturnsGeneratedFileContents keeps plan honest: it reports files
// that are not on disk, so its findings can only send an agent to the
// contents it returns.
func TestMCPPlanReturnsGeneratedFileContents(t *testing.T) {
	h := connectMCP(t, false)
	var out planOutput
	if msg := h.call(t, "plan", map[string]any{"config": fakeSpec(t), "target": "fake"}, &out); msg != "" {
		t.Fatal(msg)
	}
	if len(out.Files) != 1 || out.Files[0].Contents != "generated: for review\n" {
		t.Fatalf("plan files = %+v", out.Files)
	}
	if _, err := os.Stat(out.Files[0].Path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("plan must not write %s (stat error = %v)", out.Files[0].Path, err)
	}
}

// TestMCPReadOnlyToolsWriteNothing guards the read-only hint: a host may run
// these tools without asking the user, so none of them may leave anything in
// the user's project.
func TestMCPReadOnlyToolsWriteNothing(t *testing.T) {
	h := connectMCP(t, false)
	onTarget := func(config string) map[string]any {
		return map[string]any{"config": config, "target": "fake"}
	}
	// Arguments per tool, so a new read-only tool has to be covered here
	// before this test can pass.
	args := map[string]func(config string) map[string]any{
		"targets":          func(string) map[string]any { return nil },
		"detect":           func(c string) map[string]any { return map[string]any{"dir": filepath.Dir(c)} },
		"validate":         func(c string) map[string]any { return map[string]any{"config": c} },
		"plan":             onTarget,
		"status":           onTarget,
		"logs":             onTarget,
		"diagnose_context": onTarget,
	}

	listed, err := h.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	covered := 0
	for _, tool := range listed.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			continue
		}
		newArgs, ok := args[tool.Name]
		if !ok {
			t.Errorf("read-only tool %q is not covered here; add its arguments", tool.Name)
			continue
		}
		covered++
		t.Run(tool.Name, func(t *testing.T) {
			config := fakeSpec(t)
			dir := filepath.Dir(config)
			before := tree(t, dir)
			// A failed call is fine; the point is that nothing was written.
			h.call(t, tool.Name, newArgs(config), nil)
			if after := tree(t, dir); !slices.Equal(before, after) {
				t.Errorf("%s changed the project directory:\nbefore %v\nafter  %v", tool.Name, before, after)
			}
		})
	}
	if covered != len(args) {
		t.Errorf("checked %d read-only tools, expected %d", covered, len(args))
	}
}

// tree lists every path under dir, relative to it and sorted.
func tree(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel != "." {
			paths = append(paths, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	return paths
}

// An agent's working directory need not be the server's: the server says
// where it runs, and a missing spec is reported by the path that was tried.
func TestMCPSaysWhereItLooks(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	wd, err := os.Getwd() // the temp dir with symlinks resolved, as the server sees it
	if err != nil {
		t.Fatal(err)
	}
	h := connectMCP(t, false)
	if got := h.session.InitializeResult().Instructions; !strings.Contains(got, "This server runs in "+wd+".") {
		t.Errorf("instructions do not name the working directory %s:\n%s", wd, got)
	}

	missing := filepath.Join(wd, spec.Filename) + " does not exist; draft a spec with the detect tool"
	var valid validateOutput
	if msg := h.call(t, "validate", nil, &valid); msg != "" || valid.Valid || len(valid.Problems) != 1 || !strings.HasPrefix(valid.Problems[0], missing) {
		t.Errorf("validate: %q %+v, want a problem starting with %q", msg, valid, missing)
	}
	if msg := h.call(t, "plan", map[string]any{"target": "fake"}, nil); !strings.Contains(msg, missing) {
		t.Errorf("plan: %q, want it to contain %q", msg, missing)
	}
	for _, text := range []string{strings.Join(valid.Problems, " "), h.call(t, "plan", map[string]any{"target": "fake"}, nil)} {
		if strings.Contains(text, "anyship init") {
			t.Errorf("an agent has no shell to run `anyship init` in: %q", text)
		}
	}
}

func TestMCPDetectInARepositoryOfApps(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	api := filepath.Join(wd, "apps", "api")
	if err := os.MkdirAll(api, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(api, "go.mod"), []byte("module api\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(api, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := connectMCP(t, false)

	var root detectOutput
	if msg := h.call(t, "detect", nil, &root); msg != "" {
		t.Fatal(msg)
	}
	if root.Dir != wd || len(root.Findings) == 0 || !strings.Contains(root.Findings[0].Message, "look like projects: apps/api.") {
		t.Errorf("detect at the root: dir %q, findings %+v", root.Dir, root.Findings)
	}
	var app detectOutput
	if msg := h.call(t, "detect", map[string]any{"dir": "apps/api"}, &app); msg != "" {
		t.Fatal(msg)
	}
	if app.Dir != api || !app.Valid {
		t.Errorf("detect in apps/api: dir %q, valid %v, problems %v", app.Dir, app.Valid, app.Problems)
	}
}

// The first seven are messages adapters and detection produce today; the
// rest cover the other forms the rule handles and the ones it must leave alone.
func TestAgentTextNamesToolsNotCommands(t *testing.T) {
	for in, want := range map[string]string{
		"reading logs from h failed (exit status 255); has shop been deployed there with `anyship apply -t vps`?": "reading logs from h failed (exit status 255); has shop been deployed there with the apply tool?",
		"shop-web isn't deployed; deploy it with `anyship apply -t aws`":                                          "shop-web isn't deployed; deploy it with the apply tool",
		"New tasks roll out over a few minutes; `anyship status -t aws` shows progress.":                          "New tasks roll out over a few minutes; the status tool shows progress.",
		"Pass the other service's URL in env (anyship status shows it after the first deploy).":                   "Pass the other service's URL in env (the status tool shows it after the first deploy).",
		"Add gunicorn (or uvicorn for ASGI apps) to your dependencies and run `anyship init --force`.":            "Add gunicorn (or uvicorn for ASGI apps) to your dependencies and run the detect tool.",
		"Run `anyship init` in the member crate to deploy, or set services.web.path to it.":                       "Run the detect tool in the member crate to deploy, or set services.web.path to it.",
		"Keep the volumes data and the secrets in h:app; destroy --volumes deletes them.":                         "Keep the volumes data and the secrets in h:app; destroy with volumes=true deletes them.",
		"Removed nothing; `anyship destroy -t vps --volumes` also deletes data.":                                  "Removed nothing; the destroy tool with volumes=true also deletes data.",
		"Check the host first with `anyship apply -t vps --dry-run`.":                                             "Check the host first with the apply tool with dry_run=true.",
		"Pin it by digest, or override it for one deploy: `anyship apply --image web=<ref>`.":                     "Pin it by digest, or override it for one deploy: the apply tool with its images argument.",
		"Ask Claude with `anyship diagnose -t vps`.":                                                              "Ask Claude with the diagnose_context tool.",
		// Advice about other programs, and about this server, is the user's to act on.
		"Check that `ssh -p 2222 deploy@h` works without a password prompt.":                                   "Check that `ssh -p 2222 deploy@h` works without a password prompt.",
		"this anyship MCP server only allows dry runs; restart it with `anyship mcp --allow-deploy` to deploy": "this anyship MCP server only allows dry runs; restart it with `anyship mcp --allow-deploy` to deploy",
		"No Dockerfile, so anyship generated one; review it in the plan's generated files.":                    "No Dockerfile, so anyship generated one; review it in the plan's generated files.",
		"Enable the API with `gcloud services enable run.googleapis.com`.":                                     "Enable the API with `gcloud services enable run.googleapis.com`.",
	} {
		if got := agentText(in); got != want {
			t.Errorf("agentText(%q)\n got  %q\n want %q", in, got, want)
		}
	}
}

// The rewording reaches what tools return: findings, and errors.
func TestMCPWordsMessagesForAnAgent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("from flask import Flask\napp = Flask(__name__)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := connectMCP(t, false)
	var out detectOutput
	if msg := h.call(t, "detect", map[string]any{"dir": dir}, &out); msg != "" {
		t.Fatal(msg)
	}
	var hints []string
	for _, f := range out.Findings {
		hints = append(hints, f.Hint)
	}
	if all := strings.Join(hints, "\n"); !strings.Contains(all, "run the detect tool") || strings.Contains(all, "anyship init") {
		t.Errorf("detect hints over MCP: %q", hints)
	}
	if err := forAgent(errors.New("has it been deployed with `anyship apply -t gcp`?")); err.Error() != "has it been deployed with the apply tool?" {
		t.Errorf("error over MCP: %v", err)
	}
	if forAgent(nil) != nil {
		t.Error("no error must stay no error")
	}
}

// The schema and the target pages are resources, so an agent can read what
// an option hint points at instead of a URL.
func TestMCPResources(t *testing.T) {
	h := connectMCP(t, false)
	ctx := context.Background()
	listed, err := h.session.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var uris []string
	for _, r := range listed.Resources {
		uris = append(uris, r.URI)
	}
	slices.Sort(uris)
	if want := []string{"anyship://schema", "anyship://targets/cloudflare", "anyship://targets/vps"}; !slices.Equal(uris, want) {
		t.Errorf("resources = %v, want %v (the fake target has no page)", uris, want)
	}
	schema, err := h.session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "anyship://schema"})
	if err != nil {
		t.Fatal(err)
	}
	if len(schema.Contents) != 1 || !strings.Contains(schema.Contents[0].Text, `"$schema"`) || !strings.Contains(schema.Contents[0].Text, `"services"`) {
		t.Errorf("schema resource = %+v", schema.Contents)
	}
	page, err := h.session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "anyship://targets/vps"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Contents) != 1 || page.Contents[0].MIMEType != "text/markdown" || !strings.HasPrefix(page.Contents[0].Text, "# The `vps` target") {
		t.Errorf("vps page = %+v", page.Contents)
	}
	if _, err := h.session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "anyship://targets/fake"}); err == nil {
		t.Error("a target without a page shouldn't have a resource")
	}
	hint := agentText(adapter.OptionsHint("vps", "`host: deploy@203.0.113.10`"))
	if !strings.HasSuffix(hint, "Every option: the anyship://targets/vps resource") {
		t.Errorf("hint for an agent = %q", hint)
	}
}
