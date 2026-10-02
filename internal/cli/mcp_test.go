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
	}
	env.Logf("$ deploying")
	if err := env.Exec(ctx, adapter.ExecOptions{}, "sh", "-c", "echo from-stdout; echo from-stderr >&2; cat"); err != nil {
		return &adapter.Result{Messages: []string{err.Error()}}, nil
	}
	return &adapter.Result{OK: true, Messages: []string{"deployed"}}, nil
}

func (*fakeAdapter) Logs(ctx context.Context, _ *spec.Spec, env *adapter.Env, opts adapter.LogOptions) error {
	return env.Exec(ctx, adapter.ExecOptions{}, "sh", "-c", "echo log line for "+opts.Service)
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
		}
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
	bin := filepath.Join(t.TempDir(), "anyship")
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
