package spec

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// app wraps a YAML spec body in the manifest envelope.
func app(name, body string) []byte {
	return []byte("apiVersion: anyship/v1alpha1\nkind: App\nmetadata:\n  name: " + name + "\nspec:\n" + body)
}

func mustParse(t *testing.T, src []byte) *Spec {
	t.Helper()
	s, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return s
}

func problemsOf(t *testing.T, src []byte) []string {
	t.Helper()
	_, err := Parse(src)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("want a ValidationError, got %v", err)
	}
	return verr.Problems
}

func TestParseFillsDefaults(t *testing.T) {
	s := mustParse(t, app("app", `
  # comments are fine
  services:
    web:
      kind: server
      start: node index.js
      ports:
        - port: 80
      volumes:
        - {name: data, mountPath: /data, size: 1GB}
`))
	web := s.Services["web"]
	if web.Path != "." || web.Replicas != 1 || web.Runtime == nil || web.Env == nil {
		t.Errorf("defaults not applied: %+v", web)
	}
	if web.Ports[0].Protocol != ProtocolHTTP || web.Ports[0].Exposure != ExposurePublic {
		t.Errorf("port defaults not applied: %+v", web.Ports[0])
	}
	if web.Volumes[0].Class != "standard" {
		t.Errorf("volume class default not applied: %+v", web.Volumes[0])
	}
	if s.Resources == nil || s.Secrets == nil || s.Targets == nil {
		t.Error("top-level maps should be non-nil")
	}
}

func TestParseRejectsDanglingReferences(t *testing.T) {
	got := problemsOf(t, app("app", `
  services:
    web: {kind: server, start: x, uses: [db], secrets: [API_KEY], dependsOn: [web, missing]}
`))
	want := []string{
		`spec.services.web.secrets.0: secret "API_KEY" is not declared in spec.secrets`,
		`spec.services.web.uses.0: unknown resource "db"`,
		"spec.services.web.dependsOn.0: a service cannot depend on itself",
		`spec.services.web.dependsOn.1: unknown service "missing"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("problems:\n got  %q\n want %q", got, want)
	}
}

func TestParseRequiresAWayToRun(t *testing.T) {
	got := problemsOf(t, app("app", "  services:\n    web: {kind: server}\n"))
	if !slices.Contains(got, "spec.services.web: needs one of start, entry, image or dockerfile") {
		t.Errorf("problems: %q", got)
	}
}

func TestParseRejectsBadNamesAndDuplicatePorts(t *testing.T) {
	got := problemsOf(t, app(`"My App"`, "  services:\n    web: {kind: server, start: x, ports: [{port: 80}, {port: 80}]}\n"))
	if !slices.ContainsFunc(got, func(p string) bool { return strings.HasPrefix(p, "metadata.name:") }) {
		t.Errorf("want a name problem, got %q", got)
	}
	if !slices.Contains(got, "spec.services.web.ports.1.port: port 80 is listed twice") {
		t.Errorf("want a duplicate port problem, got %q", got)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse(app("app", "  services:\n    web: {kind: server, start: x, replica: 2}\n"))
	if err == nil || !strings.Contains(err.Error(), `unknown field "replica"`) {
		t.Errorf("want unknown field error, got %v", err)
	}
}

func TestParseChecksTheEnvelope(t *testing.T) {
	got := problemsOf(t, []byte("apiVersion: v1\nkind: Deployment\nmetadata: {name: app}\n"))
	want := []string{"apiVersion: must be anyship/v1alpha1", "kind: must be App", "spec: is required"}
	if !slices.Equal(got, want) {
		t.Errorf("problems:\n got  %q\n want %q", got, want)
	}
	if _, err := Parse([]byte("apiVersion: anyship/v1alpha1\nkind: App\nkind: App\n")); err == nil || !strings.Contains(err.Error(), "appears twice") {
		t.Errorf("want a duplicate key error, got %v", err)
	}
}

func TestMarshalRoundTrips(t *testing.T) {
	s := mustParse(t, app("shop", `
  services:
    web: {kind: server, image: "nginx:1.27", env: {PORT: "8080", DEBUG: "true"}}
  targets:
    vps: {host: deploy@203.0.113.10}
`))
	data, err := Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("# yaml-language-server: $schema="+SchemaURL+"\napiVersion: anyship/v1alpha1\nkind: App\nmetadata:\n  name: shop\nspec:\n")) {
		t.Errorf("unexpected header:\n%s", data)
	}
	again := mustParse(t, data)
	if again.Services["web"].Env["PORT"] != "8080" || again.Services["web"].Env["DEBUG"] != "true" || string(again.Targets["vps"]) != `{"host":"deploy@203.0.113.10"}` {
		t.Errorf("round trip changed the spec:\n%s", data)
	}
}

// A spec written on Windows may use backslashes. Normalize rewrites them, so
// every adapter sees one slash-separated form and the spec means the same thing
// on every machine. The result must not depend on the local separator, which is
// why this test runs everywhere, not only on Windows.
func TestParseNormalizesWindowsSeparators(t *testing.T) {
	s := mustParse(t, app("app", `
  services:
    site:
      kind: static
      path: apps\site
      build: {command: npm run build, output: dist\assets}
    api:
      kind: server
      path: services\api
      dockerfile: docker\Dockerfile
      entry: src\index.ts
`))
	site, api := s.Services["site"], s.Services["api"]
	if site.Path != "apps/site" || site.Build.Output != "dist/assets" {
		t.Errorf("path = %q, output = %q", site.Path, site.Build.Output)
	}
	if api.Path != "services/api" || api.Dockerfile != "docker/Dockerfile" || api.Entry != "src/index.ts" {
		t.Errorf("path = %q, dockerfile = %q, entry = %q", api.Path, api.Dockerfile, api.Entry)
	}
}

func TestLoadPointsAtTheOldJSONFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "anyship.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(filepath.Join(dir, Filename))
	if err == nil || !strings.Contains(err.Error(), "YAML manifests now") {
		t.Errorf("want a pointer to the new format, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), MigrationGuide) {
		t.Errorf("want a pointer to %s, got %v", MigrationGuide, err)
	}
}

type fencedBlock struct {
	lang string
	body string
}

// fencedBlocks returns the fenced code blocks of a Markdown file, in order.
func fencedBlocks(t *testing.T, path string) []fencedBlock {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var blocks []fencedBlock
	var open *fencedBlock
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case !strings.HasPrefix(line, "```") && open != nil:
			open.body += line + "\n"
		case !strings.HasPrefix(line, "```"):
		case open == nil:
			open = &fencedBlock{lang: strings.TrimSpace(strings.TrimPrefix(line, "```"))}
		default:
			blocks, open = append(blocks, *open), nil
		}
	}
	if open != nil {
		t.Fatalf("%s: a code fence is never closed", path)
	}
	return blocks
}

// The migration guide claims its "after" specs are valid and its "before" specs
// are not. Check both, so the guide can't drift from the code.
func TestMigrationGuideExamples(t *testing.T) {
	var current, legacy int
	for _, block := range fencedBlocks(t, filepath.Join("..", MigrationGuide)) {
		switch {
		case block.lang == "yaml" && strings.Contains(block.body, "apiVersion:"):
			current++
			mustParse(t, []byte(block.body))
		case block.lang == "json" && strings.Contains(block.body, `"version": 1`):
			legacy++
			if _, err := Parse([]byte(block.body)); err == nil {
				t.Errorf("a v0.2 example is still accepted:\n%s", block.body)
			}
		}
	}
	if current < 2 || legacy < 2 {
		t.Errorf("want at least two before/after pairs, got %d v0.3 and %d v0.2 examples", current, legacy)
	}
}

func TestExamplesAreValid(t *testing.T) {
	for _, example := range []string{"hono-worker", "ethereum-node"} {
		t.Run(example, func(t *testing.T) {
			if _, err := Load(filepath.Join("..", "examples", example, Filename)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The checked-in schema must match the Go types. Regenerate it with:
//
//	go run ./cmd/anyship schema > schema/anyship.schema.json
func TestCheckedInJSONSchemaIsCurrent(t *testing.T) {
	want, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join("..", "schema", "anyship.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("schema/anyship.schema.json is stale; regenerate it with `go run ./cmd/anyship schema > schema/anyship.schema.json`")
	}
}
