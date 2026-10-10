package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
