package cli

import (
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

func TestPlanJSONUsesEmptyLists(t *testing.T) {
	var buf strings.Builder
	if err := printJSON(&buf, planJSON(&adapter.Plan{Target: "vps"})); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"target\": \"vps\",\n  \"ready\": true,\n  \"findings\": [],\n  \"actions\": [],\n  \"files\": []\n}\n"
	if buf.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}
