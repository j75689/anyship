// Package spectest lets tests write specs compactly: the app as one JSON
// object with a top-level "name", which Wrap puts in the anyship.yaml envelope.
package spectest

import (
	"encoding/json"
	"fmt"

	"github.com/j75689/anyship/spec"
)

// Wrap turns {"name": "app", "services": ...} into a full manifest.
func Wrap(src string) []byte {
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(src), &body); err != nil {
		panic(fmt.Sprintf("spectest: %v in %s", err, src))
	}
	var name string
	_ = json.Unmarshal(body["name"], &name)
	delete(body, "name")
	out, err := json.Marshal(map[string]any{
		"apiVersion": spec.APIVersion, "kind": spec.Kind,
		"metadata": map[string]string{"name": name}, "spec": body,
	})
	if err != nil {
		panic(err)
	}
	return out
}

// Parse wraps src and parses it.
func Parse(src string) (*spec.Spec, error) { return spec.Parse(Wrap(src)) }
