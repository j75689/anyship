package spec

import (
	"bytes"
	"errors"
	"fmt"
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

// withDomains is a one-service spec whose public http port can host domains.
func withDomains(domains string) []byte {
	return app("app", "  services:\n    web: {kind: server, start: x, ports: [{port: 8080}], domains: "+domains+"}\n")
}

func TestParseNormalizesDomains(t *testing.T) {
	s := mustParse(t, withDomains(`["App.Example.COM", "shop.example.com.", " api.example.com "]`))
	want := []string{"app.example.com", "shop.example.com", "api.example.com"}
	if !slices.Equal(s.Services["web"].Domains, want) {
		t.Errorf("domains = %q, want %q", s.Services["web"].Domains, want)
	}
}

func TestParseRejectsBadDomains(t *testing.T) {
	for _, tc := range []struct{ name, domains, want string }{
		{"wildcard", `["*.example.com"]`, `wildcard domains are not supported; name each host, e.g. "app.example.com"`},
		{"no dot", `[example]`, `invalid domain "example": ` + domainHint},
		{"underscore", `[under_score.example.com]`, `invalid domain "under_score.example.com": ` + domainHint},
		{"with scheme", `["https://example.com"]`, `invalid domain "https://example.com": ` + domainHint},
		{"with port", `["example.com:8080"]`, `invalid domain "example.com:8080": ` + domainHint},
		{"with path", `["example.com/app"]`, `invalid domain "example.com/app": ` + domainHint},
		{"empty", `[""]`, `invalid domain "": ` + domainHint},
		{"numeric tld", `[example.123]`, `invalid domain "example.123": ` + domainHint},
		{"too long", `["` + strings.Repeat("label.", 45) + `example.com"]`, "is longer than 253 characters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := problemsOf(t, withDomains(tc.domains))
			if want := "spec.services.web.domains.0: " + tc.want; !slices.Contains(got, want) {
				t.Errorf("problems:\n got  %q\n want %q", got, want)
			}
		})
	}
}

func TestParseRejectsDuplicateDomains(t *testing.T) {
	got := problemsOf(t, app("app", `
  services:
    api:
      kind: server
      start: x
      ports: [{port: 8080}]
      domains: [shop.example.com, SHOP.example.com]
    web:
      kind: server
      start: x
      ports: [{port: 8080}]
      domains: [shop.example.com]
`))
	want := []string{
		`spec.services.api.domains.1: domain "shop.example.com" is listed twice`,
		`spec.services.web.domains.0: domain "shop.example.com" is already claimed by service "api"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("problems:\n got  %q\n want %q", got, want)
	}
}

func TestParseRequiresAPublicHTTPPortForDomains(t *testing.T) {
	const want = `spec.services.%s.domains: needs a port with protocol "http" and exposure "public" to route to`
	for name, body := range map[string]string{
		"no-ports": "{kind: server, start: x, domains: [a.example.com]}",
		"internal": "{kind: server, start: x, ports: [{port: 8080, exposure: internal}], domains: [b.example.com]}",
		"tcp":      "{kind: server, start: x, ports: [{port: 8080, protocol: tcp}], domains: [c.example.com]}",
		"worker":   "{kind: worker, start: x, domains: [d.example.com]}",
	} {
		t.Run(name, func(t *testing.T) {
			got := problemsOf(t, app("app", "  services:\n    "+name+": "+body+"\n"))
			if w := fmt.Sprintf(want, name); !slices.Contains(got, w) {
				t.Errorf("problems:\n got  %q\n want %q", got, w)
			}
		})
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
	if err == nil || !strings.Contains(err.Error(), ChangelogURL) {
		t.Errorf("want a pointer to %s, got %v", ChangelogURL, err)
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
