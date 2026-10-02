package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/adapters/cloudflare"
	"github.com/j75689/anyship/adapters/vps"
	"github.com/j75689/anyship/diagnose"
)

// cannedModel answers every request with the same diagnosis.
type cannedModel struct {
	answer *diagnose.Diagnosis
	calls  int
}

func (m *cannedModel) Diagnose(context.Context, diagnose.Request) (*diagnose.Diagnosis, error) {
	m.calls++
	return m.answer, nil
}

func runCLI(t *testing.T, model diagnose.Model, args ...string) (string, error) {
	t.Helper()
	registry, err := adapter.NewRegistry(cloudflare.New(), vps.New(), &fakeAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{registry: registry, out: &out, style: styler{}, newModel: func(string, string) diagnose.Model { return model }}
	root := a.rootCommand("test")
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	err = root.ExecuteContext(context.Background())
	return out.String(), err
}

func TestDiagnoseShowContextDoesNotCallTheModel(t *testing.T) {
	model := &cannedModel{}
	out, err := runCLI(t, model, "diagnose", "-t", "fake", "-c", fakeSpec(t), "--show-context", "--note", "502 from the proxy")
	if err != nil {
		t.Fatal(err)
	}
	if model.calls != 0 {
		t.Error("--show-context must not call the model")
	}
	for _, want := range []string{"## anyship.json", "502 from the proxy", "## Target checks (dry run)", "from-stdout", "log line for"} {
		if !strings.Contains(out, want) {
			t.Errorf("context is missing %q:\n%s", want, out)
		}
	}
}

func TestDiagnoseAppliesAValidatedPatch(t *testing.T) {
	config := fakeSpec(t)
	model := &cannedModel{answer: &diagnose.Diagnosis{
		Summary: "The image tag is wrong.", RootCause: "nginx:latest moved", Confidence: "medium",
		Evidence: []string{"pull error"}, Steps: []string{"pin the tag"},
		SpecPatch: []diagnose.PatchOp{{Op: "replace", Path: "/services/web/image", Value: `"nginx:1.27"`}},
	}}
	out, err := runCLI(t, model, "diagnose", "-t", "fake", "-c", config, "--yes")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Diagnosis: The image tag is wrong.", "Root cause (medium confidence)", "1. pin the tag", `replace /services/web/image: "nginx" → "nginx:1.27"`, "✔ Updated"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	data, _ := os.ReadFile(config)
	if !strings.Contains(string(data), `"image": "nginx:1.27"`) {
		t.Errorf("anyship.json was not updated:\n%s", data)
	}
}

func TestDiagnoseNeverWritesAnInvalidPatch(t *testing.T) {
	config := fakeSpec(t)
	before, _ := os.ReadFile(config)
	model := &cannedModel{answer: &diagnose.Diagnosis{
		Summary:   "x",
		SpecPatch: []diagnose.PatchOp{{Op: "replace", Path: "/services/web/kind", Value: `"lambda"`}},
	}}
	out, err := runCLI(t, model, "diagnose", "-t", "fake", "-c", config, "--yes")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(config)
	if !bytes.Equal(before, after) {
		t.Error("an invalid patch must not be written")
	}
	if model.calls != diagnose.MaxAttempts || !strings.Contains(out, "didn't validate") {
		t.Errorf("calls = %d, output:\n%s", model.calls, out)
	}
}

func TestDiagnoseJSON(t *testing.T) {
	model := &cannedModel{answer: &diagnose.Diagnosis{Summary: "host is out of disk", Confidence: "high"}}
	out, err := runCLI(t, model, "diagnose", "-t", "fake", "-c", fakeSpec(t), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Diagnosis  diagnose.Diagnosis `json:"diagnosis"`
		ValidPatch bool               `json:"valid_patch"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got.Diagnosis.Summary != "host is out of disk" || got.ValidPatch {
		t.Errorf("got %+v", got)
	}
}
