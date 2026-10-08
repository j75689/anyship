package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
)

func TestValidSince(t *testing.T) {
	for s, want := range map[string]bool{
		"10m":                  true,
		"1h30m":                true,
		"2026-10-01":           true,
		"2026-10-01T12:00:00Z": true,
		"0s":                   false,
		"-5m":                  false,
		"yesterday":            false,
		"10m; rm -rf /":        false,
	} {
		if got := validSince(s); got != want {
			t.Errorf("validSince(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestPrintStatus(t *testing.T) {
	st := &adapter.Status{Target: "vps", Location: "h:anyship/app", Deployed: true, Services: []adapter.ServiceStatus{
		{Name: "api", State: "running", Health: "healthy", Running: 2, Desired: 2, Ports: []string{"8080/tcp"}, Detail: "Up 1 hour"},
		{Name: "worker", State: "exited", Running: 0, Desired: 1},
	}}
	var buf strings.Builder
	printStatus(&buf, styler{}, "app", st)
	got := buf.String()
	for _, want := range []string{
		"app on vps (h:anyship/app)\n",
		"  SERVICE  STATE    HEALTH   RUNNING  PORTS     DETAIL\n",
		"  api      running  healthy  2/2      8080/tcp  Up 1 hour\n",
		"  worker   exited   -        0/1      -         -\n",
		"Some services aren't running",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}

	buf.Reset()
	printStatus(&buf, styler{}, "app", &adapter.Status{Target: "vps", Location: "h:x"})
	if !strings.Contains(buf.String(), "Not deployed.") {
		t.Errorf("output:\n%s", buf.String())
	}
}

// TestRunWithPassesArgumentsThrough guards the Windows path through runWith:
// it used to wrap every command in `cmd /C`, which let cmd.exe re-parse the
// arguments. These are the characters cmd.exe treats as special, and adapters
// really send them: the vps target hands ssh a whole shell script, and the aws
// target passes JSON on the command line.
func TestRunWithPassesArgumentsThrough(t *testing.T) {
	want := []string{
		`cd app && docker compose -f "compose.yaml" up -d`,
		`[{"Key":"anyship","Value":"shop & co"}]`,
		"a^b|c<d>e%f",
	}
	opts, name, args := helper("echo-args", want...)
	var out bytes.Buffer
	if err := runWith(context.Background(), opts, stdio{in: bytes.NewReader(nil), out: &out, err: &out}, name, args...); err != nil {
		t.Fatalf("runWith: %v\n%s", err, out.String())
	}
	got := strings.Split(strings.TrimSuffix(strings.ReplaceAll(out.String(), "\r\n", "\n"), "\n"), "\n")
	if !slices.Equal(got, want) {
		t.Errorf("the child received\n%q\nwant\n%q", got, want)
	}
}

func TestPlanJSONUsesEmptyLists(t *testing.T) {
	var buf strings.Builder
	if err := printJSON(&buf, newPlanOutput(&adapter.Plan{Target: "vps"}, shell)); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"target\": \"vps\",\n  \"ready\": true,\n  \"findings\": [],\n  \"actions\": [],\n  \"files\": []\n}\n"
	if buf.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

// plan writes nothing, so a script reading --json gets what the files would
// hold, not only paths that do not exist yet.
func TestPlanJSONCarriesGeneratedFileContents(t *testing.T) {
	var buf strings.Builder
	p := &adapter.Plan{Target: "vps", Files: []adapter.File{{Path: "/app/.anyship/vps/compose.yaml", Contents: []byte("services: {}\n")}}}
	if err := printJSON(&buf, newPlanOutput(p, shell)); err != nil {
		t.Fatal(err)
	}
	if want := "\"files\": [\n    {\n      \"path\": \"/app/.anyship/vps/compose.yaml\",\n      \"contents\": \"services: {}\\n\"\n    }\n  ]"; !strings.Contains(buf.String(), want) {
		t.Errorf("got:\n%s\nwant it to contain:\n%s", buf.String(), want)
	}
}
