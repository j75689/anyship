package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// The embedded copy is what ships; the file is what reviewers read. A wrong
// //go:embed path would make them differ.
func TestEmbeddedSchemaMatchesTheFile(t *testing.T) {
	onDisk, err := os.ReadFile(Filename)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(JSON, onDisk) {
		t.Errorf("the embedded schema differs from %s", Filename)
	}
}

func TestEmbeddedSchemaIsJSON(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(JSON, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["$schema"] == nil {
		t.Error(`the schema has no "$schema" key`)
	}
}
