package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/adapters/aws"
	"github.com/j75689/anyship/adapters/cloudflare"
	"github.com/j75689/anyship/adapters/gcp"
	"github.com/j75689/anyship/adapters/kubernetes"
	"github.com/j75689/anyship/adapters/vps"
	"github.com/j75689/anyship/spec"
)

// flaskProject is a directory init can draft a spec for.
func flaskProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"requirements.txt": "flask\n", "app.py": "from flask import Flask\napp = Flask(__name__)\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func decode[T any](t *testing.T, out string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return v
}

func TestInitStdoutPrintsTheDraftAndWritesNothing(t *testing.T) {
	dir := flaskProject(t)
	out, err := runCLI(t, "init", "--stdout", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "# yaml-language-server:") || !strings.Contains(out, "apiVersion: anyship/v1alpha1") || strings.Contains(out, "Detected:") {
		t.Errorf("stdout should carry the draft alone:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, spec.Filename)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--stdout must not write %s: %v", spec.Filename, err)
	}
}

func TestInitJSONMatchesTheDetectTool(t *testing.T) {
	dir := flaskProject(t)
	out, err := runCLI(t, "init", "--json", dir)
	if err != nil {
		t.Fatal(err)
	}
	got := decode[detectOutput](t, out)
	if got.Dir != dir || !got.Valid || !strings.Contains(got.Spec, "kind: server") || len(got.Evidence) == 0 || got.Existing != nil {
		t.Errorf("init --json = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, spec.Filename)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--json must not write %s: %v", spec.Filename, err)
	}

	// With a spec in place, init reports it instead of refusing.
	config := fakeSpec(t)
	before, _ := os.ReadFile(config)
	// The directory holds nothing to detect, so the draft is incomplete and
	// init exits non-zero, after printing.
	out, err = runCLI(t, "init", "--json", filepath.Dir(config))
	if !errors.Is(err, errReported) {
		t.Fatal(err)
	}
	got = decode[detectOutput](t, out)
	if got.Valid || got.Existing == nil || !got.Existing.Valid || got.Existing.Name != "demo" || !reflect.DeepEqual(got.Existing.Services, []string{"web"}) {
		t.Errorf("existing = %+v", got.Existing)
	}
	if after, _ := os.ReadFile(config); string(after) != string(before) {
		t.Error("init --json changed the existing spec")
	}
	if _, err := runCLI(t, "init", filepath.Dir(config)); err == nil || !strings.Contains(err.Error(), "--stdout") {
		t.Errorf("init without flags should still refuse to overwrite: %v", err)
	}
}

func TestValidateJSON(t *testing.T) {
	out, err := runCLI(t, "validate", "--json", "-c", fakeSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	got := decode[validateOutput](t, out)
	if !got.Valid || got.Name != "demo" || !reflect.DeepEqual(got.Services, []string{"web"}) || len(got.Problems) != 0 || got.Targets == nil {
		t.Errorf("validate --json = %+v", got)
	}

	bad := filepath.Join(t.TempDir(), spec.Filename)
	if err := os.WriteFile(bad, []byte("apiVersion: anyship/v1alpha1\nkind: App\nmetadata:\n  name: demo\nspec:\n  services:\n    web:\n      kind: rocket\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runCLI(t, "validate", "--json", "-c", bad)
	if !errors.Is(err, errReported) {
		t.Errorf("an invalid spec should exit non-zero after printing: %v", err)
	}
	got = decode[validateOutput](t, out)
	if got.Valid || len(got.Problems) == 0 || got.Services == nil {
		t.Errorf("validate --json on a bad spec = %+v", got)
	}

	out, err = runCLI(t, "validate", "--json", "-c", filepath.Join(t.TempDir(), spec.Filename))
	if !errors.Is(err, errReported) {
		t.Errorf("a missing spec should exit non-zero after printing: %v", err)
	}
	got = decode[validateOutput](t, out)
	if got.Valid || len(got.Problems) != 1 || !strings.Contains(got.Problems[0], "anyship init") {
		t.Errorf("validate --json on a missing spec = %+v", got)
	}
}

func TestTargetsPrintsATargetsPage(t *testing.T) {
	out, err := runCLI(t, "targets", "vps")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "# The `vps` target") {
		t.Errorf("targets vps = %q...", out[:min(len(out), 60)])
	}
	if _, err := runCLI(t, "targets", "fake"); err == nil || !strings.Contains(err.Error(), "no page") {
		t.Errorf("a target without a page: %v", err)
	}
	if _, err := runCLI(t, "targets", "nope"); err == nil {
		t.Error("an unknown target should be an error")
	}

	out, err = runCLI(t, "targets", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var docs []string
	for _, info := range decode[[]targetInfo](t, out) {
		docs = append(docs, info.Name+"="+info.Docs)
	}
	if want := []string{"cloudflare=anyship targets cloudflare", "fake=", "vps=anyship targets vps"}; !reflect.DeepEqual(docs, want) {
		t.Errorf("docs = %v, want %v", docs, want)
	}

	out, err = runCLI(t, "targets", "vps", "--json")
	if err != nil {
		t.Fatal(err)
	}
	page := decode[targetPage](t, out)
	if page.Name != "vps" || !strings.HasPrefix(page.Page, "# The `vps` target") || page.Docs != "anyship targets vps" {
		t.Errorf("targets vps --json = %+v", page.targetInfo)
	}
}

func TestApplyJSONKeepsStdoutForTheResult(t *testing.T) {
	config := fakeSpec(t)
	out, err := runCLI(t, "apply", "-t", "fake", "-c", config, "--yes", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "{") {
		t.Fatalf("stdout should carry the JSON alone:\n%s", out)
	}
	got := decode[resultOutput](t, out)
	if !got.OK || !reflect.DeepEqual(got.Messages, []string{"deployed"}) || got.Findings == nil {
		t.Errorf("apply --json = %+v", got)
	}
	for _, want := range []string{"wrote .anyship/fake/generated.yaml", "$ deploying", "from-stdout", "from-stderr"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("output is missing %q:\n%s", want, got.Output)
		}
	}
	if len(got.Files) != 1 || filepath.Base(got.Files[0]) != "generated.yaml" {
		t.Errorf("files = %v", got.Files)
	}
}

// warnSpec is fakeSpec with options that make the fake plan warn.
func warnSpec(t *testing.T) string {
	t.Helper()
	path := fakeSpec(t)
	src, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(src, "  targets:\n    fake: {warn: true}\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// codeIndex is where the finding with code is in findings, or -1.
func codeIndex(findings []adapter.Finding, code string) int {
	return slices.IndexFunc(findings, func(f adapter.Finding) bool { return f.Code == code })
}

// The plan's findings come first, so they are there even when the target's
// checks fail.
func TestApplyOutputPutsPlanFindingsFirst(t *testing.T) {
	note := adapter.Finding{Level: adapter.Info, Code: "FAKE_PLAN_NOTE", Message: "A note."}
	preflight := adapter.Finding{Level: adapter.Error, Code: "FAKE_PREFLIGHT", Message: "fakectl is missing."}
	p := &adapter.Plan{Findings: []adapter.Finding{fakePlanWarning, note}, Files: []adapter.File{{Path: "generated.yaml"}}}
	r := &adapter.Result{Findings: []adapter.Finding{preflight}, Messages: []string{"failed"}}

	out := newApplyOutput(p, r, &tailBuffer{max: maxToolOutput}, shell)
	if want := []adapter.Finding{fakePlanWarning, note, preflight}; !reflect.DeepEqual(out.Findings, want) {
		t.Errorf("findings = %+v, want %+v", out.Findings, want)
	}
	if out.OK || !reflect.DeepEqual(out.Files, []string{"generated.yaml"}) {
		t.Errorf("apply output = %+v", out)
	}
	if hint := newApplyOutput(p, r, &tailBuffer{max: maxToolOutput}, agent).Findings[0].Hint; hint != "Run the apply tool with its services argument to deploy it alone." {
		t.Errorf("agent hint = %q", hint)
	}
	if out := newApplyOutput(&adapter.Plan{}, &adapter.Result{OK: true}, &tailBuffer{max: maxToolOutput}, shell); out.Findings == nil {
		t.Error("findings should be [] when there are none")
	}
}

// apply --json carries the plan's findings on every target, ahead of the
// checks. The checks fail without the target's tools; vps gets an ssh that
// answers, since it has no check of its own when ssh is missing.
func TestApplyJSONIncludesPlanFindingsOnEveryTarget(t *testing.T) {
	path := t.TempDir()
	t.Setenv("PATH", path)
	adapters := []adapter.Adapter{gcp.New(), aws.New(), kubernetes.New(), cloudflare.New(), vps.New()}
	for _, tc := range []struct{ target, services, code string }{
		{"gcp", `    db: {kind: server, image: "db:1"}
    web: {kind: server, image: "web:1", ports: [{port: 80}], dependsOn: [db]}
  targets:
    gcp: {project: my-project, region: us-central1}`, "GCP_DEPENDS_ON"},
		{"aws", `    db: {kind: server, image: "db:1"}
    web: {kind: server, image: "web:1", dependsOn: [db]}
  targets:
    aws: {region: us-east-1}`, "AWS_DEPENDS_ON"},
		{"kubernetes", `    db: {kind: server, image: "db:1"}
    web: {kind: server, image: "web:1", dependsOn: [db]}
  targets:
    kubernetes: {namespace: demo}`, "K8S_DEPENDS_ON"},
		{"cloudflare", `    web: {kind: static, replicas: 2, build: {command: make, output: out}}`, "CF_REPLICAS_IGNORED"},
		{"vps", `    web: {kind: server, image: "web:latest"}
  targets:
    vps: {host: example.com}`, "VPS_MUTABLE_TAG"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), spec.Filename)
			src := "apiVersion: anyship/v1alpha1\nkind: App\nmetadata:\n  name: demo\nspec:\n  services:\n" + tc.services + "\n"
			if err := os.WriteFile(config, []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.target == "vps" {
				installHelper(t, path, "ssh")
			}
			out, _ := runCLIOn(t, adapters, "apply", "-t", tc.target, "-c", config, "--dry-run", "--yes", "--json")
			got := decode[resultOutput](t, out).Findings
			i := codeIndex(got, tc.code)
			if i < 0 {
				t.Fatalf("findings = %+v, want %s", got, tc.code)
			}
			if j := slices.IndexFunc(got, func(f adapter.Finding) bool { return f.Level == adapter.Error }); j < i {
				t.Errorf("findings = %+v, want %s before the failed check", got, tc.code)
			}
		})
	}

	// Over MCP too.
	var viaTool resultOutput
	if msg := connectMCP(t, false).call(t, "apply", map[string]any{"config": warnSpec(t), "target": "fake", "dry_run": true}, &viaTool); msg != "" {
		t.Fatal(msg)
	}
	if i := codeIndex(viaTool.Findings, "FAKE_PLAN_WARNING"); i < 0 || !strings.Contains(viaTool.Findings[i].Hint, "the apply tool") {
		t.Errorf("apply tool findings = %+v", viaTool.Findings)
	}
}

// The terminal already shows the plan before it asks, so apply doesn't show
// its warnings again; neither does --json.
func TestApplyTextShowsPlanWarningsOnce(t *testing.T) {
	config := warnSpec(t)
	out, err := runCLI(t, "apply", "-t", "fake", "-c", config, "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, fakePlanWarning.Message); n != 1 {
		t.Errorf("the warning shows %d times:\n%s", n, out)
	}
	out, err = runCLI(t, "apply", "-t", "fake", "-c", config, "--yes", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, f := range decode[resultOutput](t, out).Findings {
		if f.Code == fakePlanWarning.Code {
			n++
		}
	}
	if n != 1 {
		t.Errorf("apply --json has %d %s findings:\n%s", n, fakePlanWarning.Code, out)
	}
}

// answerPrompt makes stdin a terminal that answers the confirmation prompt
// with answer, if interactive, and returns what was written to stderr so far.
func answerPrompt(t *testing.T, interactive bool, answer string) func() string {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "stdin")
	if err := os.WriteFile(in, []byte(answer+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	isTerminal, oldIn, oldErr := stdinIsTerminal, os.Stdin, os.Stderr
	stdinIsTerminal = func() bool { return interactive }
	os.Stdin, os.Stderr = stdin, stderr
	t.Cleanup(func() {
		stdinIsTerminal, os.Stdin, os.Stderr = isTerminal, oldIn, oldErr
		_ = stdin.Close()
		_ = stderr.Close()
	})
	return func() string {
		b, err := os.ReadFile(stderr.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
}

// --json leaves out the plan, so before it asks it shows the plan's findings
// on stderr, and stdout still carries the JSON alone.
func TestApplyJSONShowsPlanFindingsBeforeThePrompt(t *testing.T) {
	stderr := answerPrompt(t, true, "y")
	out, err := runCLI(t, "apply", "-t", "fake", "-c", warnSpec(t), "--json")
	if err != nil {
		t.Fatal(err)
	}
	if got := decode[resultOutput](t, out); !got.OK || codeIndex(got.Findings, fakePlanWarning.Code) != 0 {
		t.Errorf("apply --json = %+v", got)
	}
	errOut := stderr()
	findings := strings.Index(errOut, "\nFindings:\n  ! "+fakePlanWarning.Message+"\n      → "+fakePlanWarning.Hint+"\n")
	prompt := strings.Index(errOut, "Deploy demo to fake? [y/N]")
	if findings < 0 || prompt < findings {
		t.Errorf("stderr should show the findings before the prompt:\n%s", errOut)
	}
}

func TestApplyPromptShowsNoExtraFindings(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interactive bool
		warn        bool
		args        []string
	}{
		{"no findings", true, false, []string{"--json"}},
		{"--yes", true, true, []string{"--json", "--yes"}},
		{"--dry-run", true, true, []string{"--json", "--dry-run"}},
		{"text", true, true, nil},
		{"not a terminal", false, true, []string{"--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stderr := answerPrompt(t, tc.interactive, "y")
			config := fakeSpec(t)
			if tc.warn {
				config = warnSpec(t)
			}
			out, err := runCLI(t, append([]string{"apply", "-t", "fake", "-c", config}, tc.args...)...)
			if !tc.interactive {
				if err == nil || !strings.Contains(err.Error(), "pass --yes") || out != "" {
					t.Errorf("apply = %q, %v; want the refusal", out, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if errOut := stderr(); strings.Contains(errOut, "Findings:") {
				t.Errorf("stderr shows findings:\n%s", errOut)
			}
			if tc.args == nil {
				if n := strings.Count(out, fakePlanWarning.Message); n != 1 {
					t.Errorf("the warning shows %d times:\n%s", n, out)
				}
			} else if tc.interactive {
				decode[resultOutput](t, out)
			}
		})
	}
}

func TestDestroyJSON(t *testing.T) {
	config := fakeSpec(t)
	out, err := runCLI(t, "destroy", "-t", "fake", "-c", config, "--dry-run", "--json")
	if err != nil {
		t.Fatal(err)
	}
	got := decode[resultOutput](t, out)
	if !got.OK || !reflect.DeepEqual(got.Summary, []string{"remove x"}) || !reflect.DeepEqual(got.Messages, []string{"Dry run: nothing was removed."}) {
		t.Errorf("destroy --dry-run --json = %+v", got)
	}

	out, err = runCLI(t, "destroy", "-t", "fake", "-c", config, "--yes", "--json")
	if err != nil {
		t.Fatal(err)
	}
	got = decode[resultOutput](t, out)
	if !got.OK || !reflect.DeepEqual(got.Summary, []string{"remove x"}) || !reflect.DeepEqual(got.Messages, []string{"removed"}) {
		t.Errorf("destroy --json = %+v", got)
	}
}

func TestStatusJSONHasTheVerdict(t *testing.T) {
	out, err := runCLI(t, "status", "-t", "fake", "-c", fakeSpec(t), "--json")
	if !errors.Is(err, errReported) {
		t.Errorf("not deployed should exit non-zero after printing: %v", err)
	}
	got := decode[statusOutput](t, out)
	if got.Healthy || got.Deployed || got.Target != "fake" || got.Services == nil {
		t.Errorf("status --json = %+v", got)
	}
}

// What the commands print as JSON is what the MCP tools return, read into
// the same types, so an agent sees the same thing either way.
func TestCommandsAndToolsAgree(t *testing.T) {
	config := fakeSpec(t)
	h := connectMCP(t, false)

	out, err := runCLI(t, "plan", "-t", "fake", "-c", config, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var viaTool planOutput
	if msg := h.call(t, "plan", map[string]any{"config": config, "target": "fake"}, &viaTool); msg != "" {
		t.Fatal(msg)
	}
	if viaCLI := decode[planOutput](t, out); !reflect.DeepEqual(viaCLI, viaTool) {
		t.Errorf("plan\n cli  %+v\n tool %+v", viaCLI, viaTool)
	}

	out, err = runCLI(t, "validate", "-c", config, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var validated validateOutput
	if msg := h.call(t, "validate", map[string]any{"config": config}, &validated); msg != "" {
		t.Fatal(msg)
	}
	if viaCLI := decode[validateOutput](t, out); !reflect.DeepEqual(viaCLI, validated) {
		t.Errorf("validate\n cli  %+v\n tool %+v", viaCLI, validated)
	}

	out, _ = runCLI(t, "status", "-t", "fake", "-c", config, "--json")
	var status statusOutput
	if msg := h.call(t, "status", map[string]any{"config": config, "target": "fake"}, &status); msg != "" {
		t.Fatal(msg)
	}
	if viaCLI := decode[statusOutput](t, out); !reflect.DeepEqual(viaCLI, status) {
		t.Errorf("status\n cli  %+v\n tool %+v", viaCLI, status)
	}
}

func TestDoctor(t *testing.T) {
	config := fakeSpec(t)
	h := connectMCP(t, false)

	out, err := runCLI(t, "doctor", "-t", "fake", "-c", config, "--json")
	if err != nil {
		t.Fatal(err)
	}
	viaCLI := decode[doctorOutput](t, out)
	if !viaCLI.OK || viaCLI.Spec == "" || len(viaCLI.Targets) != 1 || viaCLI.Targets[0].Dependencies[0].Version != "1.2.3" {
		t.Errorf("doctor = %+v", viaCLI)
	}
	var viaTool doctorOutput
	if msg := h.call(t, "doctor", map[string]any{"config": config, "target": "fake"}, &viaTool); msg != "" {
		t.Fatal(msg)
	}
	if !reflect.DeepEqual(viaCLI, viaTool) {
		t.Errorf("doctor\n cli  %+v\n tool %+v", viaCLI, viaTool)
	}

	// Without --target, the targets the spec names; a failed check exits 1.
	broken := filepath.Join(t.TempDir(), spec.Filename)
	src, _ := os.ReadFile(config)
	if err := os.WriteFile(broken, append(src, "  targets:\n    fake: {broken: true}\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runCLI(t, "doctor", "-c", broken)
	if !errors.Is(err, errReported) || !strings.Contains(out, "fakectl  1.2.3  dev") || !strings.Contains(out, "fakectl is broken.") ||
		!strings.Contains(out, "Fix the errors above to deploy to fake.") || strings.Contains(out, "vps") {
		t.Errorf("doctor: %v\n%s", err, out)
	}
	if msg := h.call(t, "doctor", map[string]any{"config": broken}, &viaTool); msg != "" || viaTool.OK || !strings.Contains(viaTool.Targets[0].Findings[0].Hint, "anyship://targets/fake") {
		t.Errorf("doctor tool: %s %+v", msg, viaTool)
	}

	// No spec is fine, unless one was asked for.
	t.Chdir(t.TempDir())
	if out, err := runCLI(t, "doctor", "-t", "fake", "--json"); err != nil || decode[doctorOutput](t, out).Spec != "" {
		t.Errorf("doctor without a spec: %v\n%s", err, out)
	}
	if _, err := runCLI(t, "doctor", "-t", "fake", "-c", "missing.yaml"); err == nil {
		t.Error("a missing -c spec was accepted")
	}
}
