package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

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
	two := 2
	st := &adapter.Status{Target: "vps", Location: "h:anyship/app", Deployed: true, Services: []adapter.ServiceStatus{
		{Name: "api", State: "running", Health: "healthy", Running: 2, Desired: 2, Ports: []string{"8080/tcp"}, Detail: "Up 1 hour", URL: "h:8080", Since: time.Now().Add(-90 * time.Minute).UTC().Format(time.RFC3339), Restarts: &two},
		{Name: "worker", State: "exited", Running: 0, Desired: 1, Events: []string{"BackOff: Back-off restarting failed container (x3)"}},
	}}
	var buf strings.Builder
	printStatus(&buf, styler{}, "app", st)
	got := buf.String()
	for _, want := range []string{
		"app on vps (h:anyship/app)\n",
		"  SERVICE  STATE    HEALTH   RUNNING  RESTARTS  SINCE   PORTS     URL     DETAIL\n",
		"  api      running  healthy  2/2      2         1h ago  8080/tcp  h:8080  Up 1 hour\n",
		"  worker   exited   -        0/1      -         -       -         -       -\n",
		"  ! worker: BackOff: Back-off restarting failed container (x3)\n",
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

func TestAgo(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for stamp, want := range map[string]string{
		"":                       "",
		"2026-10-08T11:59:30Z":   "just now",
		"2026-10-08T11:15:00Z":   "45m ago",
		"2026-10-08T03:00:00Z":   "9h ago",
		"2026-10-01T12:00:00Z":   "7d ago",
		"2026-10-08T11:00:00.5Z": "59m ago",
		"not a time":             "not a time",
	} {
		if got := ago(stamp, now); got != want {
			t.Errorf("ago(%q) = %q, want %q", stamp, got, want)
		}
	}
}

func TestMissingToolNamesItself(t *testing.T) {
	err := runWith(context.Background(), adapter.ExecOptions{}, stdio{in: bytes.NewReader(nil), out: io.Discard, err: io.Discard}, "anyship-no-such-tool", "--version")
	if !adapter.NotInstalled(err) {
		t.Fatalf("err = %v, which preflight wouldn't see as a missing tool", err)
	}
	want := "anyship-no-such-tool isn't installed on this machine; `anyship doctor` says how to install it"
	if err.Error() != want {
		t.Errorf("err = %q", err)
	}
	// Wrapped by an adapter, the missing tool is all the user is shown.
	wrapped := fmt.Errorf("reading logs for web failed (%w); has it been deployed with `anyship apply -t gcp`?", err)
	if got := causeForUser(wrapped); got.Error() != want {
		t.Errorf("shown: %q", got)
	}
	if got := forAgent(wrapped); got.Error() != "anyship-no-such-tool isn't installed on this machine; the doctor tool says how to install it" {
		t.Errorf("agent: %q", got)
	}
	if other := errors.New("exit status 1"); causeForUser(other) != other {
		t.Error("another error was replaced")
	}
}
