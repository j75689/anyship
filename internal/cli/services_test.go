package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/adapters/gcp"
	"github.com/j75689/anyship/adapters/vps"
	"github.com/j75689/anyship/spec"
)

// selectingAdapter is the fake target, able to deploy some services only.
type selectingAdapter struct{ fakeAdapter }

func (*selectingAdapter) SelectsServices() {}

func TestServiceFlagSelectsServices(t *testing.T) {
	s := imageSpec(t)
	got, err := selectServices(s, &selectingAdapter{}, []string{"web", "api"}, map[string]string{"web": digest})
	if err != nil || !slices.Equal(got, []string{"api", "web"}) {
		t.Errorf("selection = %q, %v; want api, web in order", got, err)
	}
	// Without the flag nothing is selected, which means every service, on
	// any target.
	if got, err := selectServices(s, vps.New(), nil, map[string]string{"web": digest}); err != nil || got != nil {
		t.Errorf("no --service: %q, %v", got, err)
	}
}

// The help names the flag's value, not its type.
func TestServiceFlagHelpNamesTheValue(t *testing.T) {
	a := &app{}
	for _, cmd := range []*cobra.Command{a.planCommand(), a.applyCommand()} {
		if help := cmd.Flags().FlagUsages(); !strings.Contains(help, "--service name ") {
			t.Errorf("%s --help:\n%s", cmd.Name(), help)
		}
	}
}

func TestServiceFlagRefusesWhatItCannotDo(t *testing.T) {
	for _, tc := range []struct {
		names  []string
		images map[string]string
		want   string
	}{
		{[]string{"worker"}, nil, `unknown service "worker"; services: api, web`},
		{[]string{"web", "web"}, nil, "--service names web twice"},
		{[]string{"api"}, map[string]string{"web": digest}, "--image web is for a service this run leaves out; add --service web or drop the image"},
	} {
		_, err := selectServices(imageSpec(t), &selectingAdapter{}, tc.names, tc.images)
		if err == nil || err.Error() != tc.want {
			t.Errorf("--service %q with images %v: %v, want %q", tc.names, tc.images, err, tc.want)
		}
	}
	_, err := selectServices(imageSpec(t), vps.New(), []string{"web"}, nil)
	if want := "the vps target can't deploy some services only; drop --service to deploy all of them"; err == nil || err.Error() != want {
		t.Errorf("a target that can't select: %v, want %q", err, want)
	}
}

// The refusal comes before a plan: nothing is generated for a run that
// can't do what it was asked.
func TestServiceFlagIsRefusedBeforeThePlan(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, spec.Filename)
	if err := os.WriteFile(config, []byte(`apiVersion: anyship/v1alpha1
kind: App
metadata:
  name: shop
spec:
  services:
    web:
      kind: server
      image: ghcr.io/acme/shop:main
  targets:
    vps:
      host: deploy@203.0.113.10
`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"plan"}, {"apply", "--yes"}} {
		command := args[0]
		out, err := runCLI(t, append(args, "-c", config, "-t", "vps", "--service", "web")...)
		if err == nil || !strings.Contains(err.Error(), "can't deploy some services only") {
			t.Errorf("%s: %v", command, err)
		}
		if out != "" {
			t.Errorf("%s printed a plan:\n%s", command, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".anyship")); !os.IsNotExist(err) {
		t.Errorf("files were generated: %v", err)
	}
}

func TestServicesIsACapabilityOfGCPOnly(t *testing.T) {
	if !slices.Contains(capabilities(gcp.New()), "services") {
		t.Errorf("gcp capabilities = %q", capabilities(gcp.New()))
	}
	if slices.Contains(capabilities(vps.New()), "services") {
		t.Errorf("vps capabilities = %q", capabilities(vps.New()))
	}
}

// Over MCP the selection is the services argument of plan and apply, checked
// the same way.
func TestMCPServicesArgument(t *testing.T) {
	h := connectMCP(t, false)
	config := fakeSpec(t)
	for _, tool := range []string{"plan", "apply"} {
		args := map[string]any{"config": config, "target": "fake", "services": []string{"web"}, "dry_run": true}
		if tool == "plan" {
			delete(args, "dry_run")
		}
		if msg := h.call(t, tool, args, nil); !strings.Contains(msg, "the fake target can't deploy some services only; drop the services argument") {
			t.Errorf("%s on a target that can't select: %q", tool, msg)
		}
		args["services"] = []string{"web", "web"}
		if msg := h.call(t, tool, args, nil); !strings.Contains(msg, "the services argument names web twice") {
			t.Errorf("%s with a service named twice: %q", tool, msg)
		}
		args["services"] = []string{"api"}
		if msg := h.call(t, tool, args, nil); !strings.Contains(msg, `unknown service "api"; services: web`) {
			t.Errorf("%s with an unknown service: %q", tool, msg)
		}
	}
}

// An agent that applies some services without planning first still hears
// about a secret a left-out service reads, ahead of the checks.
func TestMCPApplyServicesWarnsSharedSecretWithoutPlan(t *testing.T) {
	h := connectMCP(t, false, gcp.New())
	config := filepath.Join(t.TempDir(), spec.Filename)
	src := `apiVersion: anyship/v1alpha1
kind: App
metadata:
  name: shop
spec:
  services:
    web: {kind: server, image: "web:1", ports: [{port: 80}], secrets: [DB_URL]}
    search: {kind: server, image: "search:1", ports: [{port: 8080}], secrets: [DB_URL]}
  secrets:
    DB_URL: {}
  targets:
    gcp: {project: my-project, region: us-central1}
`
	if err := os.WriteFile(config, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DB_URL", "postgres://new")
	t.Setenv("PATH", t.TempDir()) // no gcloud: the checks fail

	var out resultOutput
	if msg := h.call(t, "apply", map[string]any{"config": config, "target": "gcp", "services": []string{"web"}, "dry_run": true}, &out); msg != "" {
		t.Fatal(msg)
	}
	i, j := codeIndex(out.Findings, "GCP_SECRET_SHARED_LEFT_OUT"), codeIndex(out.Findings, "GCP_PREFLIGHT_GCLOUD")
	if i < 0 || j < 0 || i > j || out.OK {
		t.Fatalf("apply = %+v, want GCP_SECRET_SHARED_LEFT_OUT before GCP_PREFLIGHT_GCLOUD", out)
	}
	f := out.Findings[i]
	if f.Level != adapter.Warning || !strings.Contains(f.Message, "search list(s) it too") || strings.Contains(f.Hint, "--service") ||
		!strings.Contains(f.Hint, "services argument") {
		t.Errorf("finding = %+v", f)
	}
}
