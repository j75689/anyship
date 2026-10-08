package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDiagnosePrintsTheRedactedContext(t *testing.T) {
	out, err := runCLI(t, "diagnose", "-t", "fake", "-c", fakeSpec(t), "--note", "502 from the proxy")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## anyship.yaml", "## What the user reports", "502 from the proxy", "## anyship plan", "## Target checks (dry run)", "from-stdout", "## Status", "## Recent logs", "log line for"} {
		if !strings.Contains(out, want) {
			t.Errorf("context is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Diagnosis:") || strings.Contains(out, "Apply this change") {
		t.Errorf("diagnose should only print the context:\n%s", out)
	}
}

func TestDiagnoseNoChecksSkipsTheDryRun(t *testing.T) {
	out, err := runCLI(t, "diagnose", "-t", "fake", "-c", fakeSpec(t), "--no-checks")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "## Target checks") {
		t.Errorf("--no-checks should skip the target's checks:\n%s", out)
	}
	if !strings.Contains(out, "## Recent logs") {
		t.Errorf("--no-checks should still read logs:\n%s", out)
	}
}

func TestDiagnoseJSON(t *testing.T) {
	out, err := runCLI(t, "diagnose", "-t", "fake", "-c", fakeSpec(t), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Context string `json:"context"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(got.Context, "## anyship.yaml") || !strings.Contains(got.Context, "## Target checks (dry run)") {
		t.Errorf("context = %q", got.Context)
	}
}
