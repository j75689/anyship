package detect

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, contents := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func detect(t *testing.T, files map[string]string) *Detection {
	t.Helper()
	d, err := Project(writeProject(t, files))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func codes(findings []adapter.Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.Code)
	}
	return out
}

// reparse round-trips the draft through YAML, the way `anyship init` writes it.
func reparse(t *testing.T, s *spec.Spec) error {
	t.Helper()
	data, err := spec.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	_, err = spec.Parse(data)
	return err
}

func TestHonoIsEdgeCompatible(t *testing.T) {
	d := detect(t, map[string]string{
		"package.json":   `{"name":"@acme/API","dependencies":{"hono":"^4"}}`,
		"pnpm-lock.yaml": "",
		"src/index.ts":   "import { Hono } from \"hono\";\nexport default new Hono();\n",
	})
	web := d.Spec.Services[ServiceName]
	if d.Spec.Name != "api" {
		t.Errorf("name = %q, want api", d.Spec.Name)
	}
	if web.Kind != spec.KindServer || web.Entry != "src/index.ts" || web.Framework() != "hono" {
		t.Errorf("service = %+v", web)
	}
	if web.Runtime.EdgeCompatible == nil || !*web.Runtime.EdgeCompatible {
		t.Error("hono without Node-only imports should be edge compatible")
	}
	if adapter.HasErrors(d.Findings) {
		t.Errorf("unexpected errors: %+v", d.Findings)
	}
	if err := reparse(t, d.Spec); err != nil {
		t.Errorf("draft should be a valid spec: %v", err)
	}
}

func TestNodeOnlyCodeIsEdgeIncompatible(t *testing.T) {
	d := detect(t, map[string]string{
		"package.json": `{"dependencies":{"express":"^5"},"scripts":{"start":"PORT=8080 node server.js"}}`,
		"server.js":    "const { exec } = require(\"node:child_process\");\nconst fs = require(\"fs\");\n",
	})
	web := d.Spec.Services[ServiceName]
	if web.Start != "npm run start" || len(web.Ports) != 1 || web.Ports[0].Port != 8080 {
		t.Errorf("service = %+v", web)
	}
	if !web.EdgeIncompatible() {
		t.Error("child_process should make the service edge incompatible")
	}
	got := codes(d.Findings)
	for _, want := range []string{"EDGE_UNSUPPORTED_MODULE", "EDGE_LIMITED_MODULE"} {
		if !slices.Contains(got, want) {
			t.Errorf("findings %v missing %s", got, want)
		}
	}
}

func TestDatabaseDependenciesBecomeResources(t *testing.T) {
	d := detect(t, map[string]string{
		"package.json": `{"dependencies":{"hono":"^4","pg":"^8","@aws-sdk/client-s3":"^3"}}`,
		"src/index.ts": "export default {};",
	})
	if len(d.Spec.Resources) != 2 || d.Spec.Resources["db"].Type != spec.ResourcePostgres || d.Spec.Resources["storage"].Type != spec.ResourceBucket {
		t.Errorf("resources = %+v", d.Spec.Resources)
	}
	if uses := d.Spec.Services[ServiceName].Uses; !slices.Equal(uses, []string{"db", "storage"}) {
		t.Errorf("uses = %v", uses)
	}
}

func TestViteIsStatic(t *testing.T) {
	d := detect(t, map[string]string{
		"package.json": `{"devDependencies":{"vite":"^7"},"scripts":{"build":"vite build"}}`,
		"yarn.lock":    "",
	})
	web := d.Spec.Services[ServiceName]
	if web.Kind != spec.KindStatic || web.Build == nil || web.Build.Command != "yarn run build" || web.Build.Output != "dist" {
		t.Errorf("service = %+v build = %+v", web, web.Build)
	}
}

func TestDockerfileFallback(t *testing.T) {
	d := detect(t, map[string]string{"Dockerfile": "FROM alpine\nEXPOSE 8000\n"})
	web := d.Spec.Services[ServiceName]
	if web.Dockerfile != "Dockerfile" || len(web.Ports) != 1 || web.Ports[0].Port != 8000 {
		t.Errorf("service = %+v", web)
	}
}

// At a repository root the useful answer is where the apps are.
func TestUnknownProjectPointsAtSubprojects(t *testing.T) {
	d := detect(t, map[string]string{
		"README.md":                          "# monorepo\n",
		"apps/api/requirements.txt":          "fastapi\n",
		"apps/web/package.json":              "{}",
		"apps/web/node_modules/x/go.mod":     "module x\n",
		"worker/go.mod":                      "module worker\n",
		"docs/guide/intro.md":                "",
		"node_modules/left-pad/package.json": "{}",
		".github/actions/build/Dockerfile":   "FROM alpine\n",
		"vendor/lib/go.mod":                  "module lib\n",
		"a/b/c/package.json":                 "{}",
	})
	var unknown adapter.Finding
	for _, f := range d.Findings {
		if f.Code == "DETECT_UNKNOWN" {
			unknown = f
		}
	}
	if want := "these subdirectories look like projects: apps/api, apps/web, worker."; !strings.HasSuffix(unknown.Message, want) {
		t.Errorf("message = %q, want it to end with %q", unknown.Message, want)
	}
	if !strings.Contains(unknown.Hint, "give its path as the directory") {
		t.Errorf("hint = %q", unknown.Hint)
	}
}

func TestSubprojectsAreCapped(t *testing.T) {
	files := map[string]string{}
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		files["apps/"+name+"/go.mod"] = "module " + name + "\n"
	}
	got := subprojects(writeProject(t, files))
	want := []string{"apps/a", "apps/b", "apps/c", "apps/d", "apps/e", "and 2 more"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestUnknownProjectIsReported(t *testing.T) {
	d := detect(t, map[string]string{"README.md": "# notes\n", "build.gradle": ""})
	if !slices.Contains(codes(d.Findings), "DETECT_UNKNOWN") {
		t.Errorf("findings = %v", codes(d.Findings))
	}
	// No subdirectory looks like a project, so the advice is about this one.
	if hint := d.Findings[0].Hint; !strings.HasPrefix(hint, "Add a Dockerfile") {
		t.Errorf("hint = %q", hint)
	}
	if reparse(t, d.Spec) == nil {
		t.Error("an undetectable project should not produce a deployable spec")
	}
}

func TestSpecName(t *testing.T) {
	for raw, want := range map[string]string{
		"@acme/My_App": "my-app",
		"123-service":  "service",
		"!!!":          "app",
		"web-":         "web",
	} {
		if got := specName(raw); got != want {
			t.Errorf("specName(%q) = %q, want %q", raw, got, want)
		}
	}
}
