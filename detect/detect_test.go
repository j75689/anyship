package detect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

// reparse round-trips the draft through JSON, the way `anyship init` writes it.
func reparse(t *testing.T, s *spec.Spec) error {
	t.Helper()
	data, err := json.Marshal(s)
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

func TestUnknownProjectIsReported(t *testing.T) {
	d := detect(t, map[string]string{"README.md": "# notes\n", "build.gradle": ""})
	if !slices.Contains(codes(d.Findings), "DETECT_UNKNOWN") {
		t.Errorf("findings = %v", codes(d.Findings))
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
