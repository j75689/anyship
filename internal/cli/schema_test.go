package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/j75689/anyship/schema"
)

func TestSchemaCommandPrintsTheCheckedInSchema(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "schema", schema.Filename))
	if err != nil {
		t.Fatal(err)
	}
	got, err := runCLI(t, nil, "schema")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("`anyship schema` does not match schema/%s", schema.Filename)
	}
}

func TestSchemaCommandWritesAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", schema.Filename)
	if _, err := runCLI(t, nil, "schema", "-o", path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, schema.JSON) {
		t.Errorf("%s does not match the embedded schema", path)
	}
}

// Without this the editor hint `anyship init` writes points at nothing.
func TestInitWritesTheSchemaItPointsAt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, nil, "init", dir); err != nil {
		t.Fatal(err)
	}

	manifest, err := os.ReadFile(filepath.Join(dir, "anyship.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	header, _, _ := strings.Cut(string(manifest), "\n")
	ref, ok := strings.CutPrefix(header, "# yaml-language-server: $schema=")
	if !ok {
		t.Fatalf("no schema hint in the first line: %q", header)
	}

	// The hint is relative to the spec file, which is what yaml-language-server
	// resolves an inline $schema against.
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ref)))
	if err != nil {
		t.Fatalf("the schema hint points at a file that is not there: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s is not valid JSON: %v", ref, err)
	}
	if !bytes.Equal(data, schema.JSON) {
		t.Errorf("%s does not match the embedded schema", ref)
	}
}
