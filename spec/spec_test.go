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

func mustParse(t *testing.T, src string) *Spec {
	t.Helper()
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return s
}

func problemsOf(t *testing.T, src string) []string {
	t.Helper()
	_, err := Parse([]byte(src))
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("want a ValidationError, got %v", err)
	}
	return verr.Problems
}

func TestParseFillsDefaults(t *testing.T) {
	s := mustParse(t, `{"version":1,"name":"app","services":{"web":{"kind":"server","start":"node index.js","ports":[{"port":80}],"volumes":[{"name":"data","mountPath":"/data","size":"1GB"}]}}}`)
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
	got := problemsOf(t, `{"version":1,"name":"app","services":{"web":{"kind":"server","start":"x","uses":["db"],"secrets":["API_KEY"],"dependsOn":["web","missing"]}}}`)
	want := []string{
		`services.web.secrets.0: secret "API_KEY" is not declared in top-level secrets`,
		`services.web.uses.0: unknown resource "db"`,
		"services.web.dependsOn.0: a service cannot depend on itself",
		`services.web.dependsOn.1: unknown service "missing"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("problems:\n got  %q\n want %q", got, want)
	}
}

func TestParseRequiresAWayToRun(t *testing.T) {
	got := problemsOf(t, `{"version":1,"name":"app","services":{"web":{"kind":"server"}}}`)
	if !slices.Contains(got, "services.web: needs one of start, entry, image or dockerfile") {
		t.Errorf("problems: %q", got)
	}
}

func TestParseRejectsBadNamesAndDuplicatePorts(t *testing.T) {
	got := problemsOf(t, `{"version":1,"name":"My App","services":{"web":{"kind":"server","start":"x","ports":[{"port":80},{"port":80}]}}}`)
	if !slices.ContainsFunc(got, func(p string) bool { return strings.HasPrefix(p, "name:") }) {
		t.Errorf("want a name problem, got %q", got)
	}
	if !slices.Contains(got, "services.web.ports.1.port: port 80 is listed twice") {
		t.Errorf("want a duplicate port problem, got %q", got)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(`{"version":1,"name":"app","services":{"web":{"kind":"server","start":"x","replica":2}}}`))
	if err == nil || !strings.Contains(err.Error(), `unknown field "replica"`) {
		t.Errorf("want unknown field error, got %v", err)
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
