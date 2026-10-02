// Package yamljson converts between YAML documents and JSON, so types keep a
// single set of json tags (and one JSON Schema) while files are written in YAML.
package yamljson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"go.yaml.in/yaml/v3"
)

// ToJSON converts a single YAML document to JSON. Anchors and aliases are
// expanded; duplicate keys, merge keys and non-scalar keys are rejected.
func ToJSON(data []byte) ([]byte, error) {
	root, err := Parse(data)
	if err != nil {
		return nil, err
	}
	v, err := Value(root)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// Parse reads exactly one YAML document and returns its document node.
func Parse(data []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the file is empty")
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("only one YAML document is allowed (found a second ---)")
	}
	return &doc, nil
}

// Value converts a node to plain Go values: map[string]any, []any and scalars.
func Value(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return Value(n.Content[0])
	case yaml.AliasNode:
		return Value(n.Alias)
	case yaml.ScalarNode:
		var v any
		if err := n.Decode(&v); err != nil {
			return nil, fmt.Errorf("line %d: %w", n.Line, err)
		}
		return v, nil
	case yaml.SequenceNode:
		items := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := Value(c)
			if err != nil {
				return nil, err
			}
			items = append(items, v)
		}
		return items, nil
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, val := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: keys must be plain values", k.Line)
			}
			if k.Tag == "!!merge" {
				return nil, fmt.Errorf("line %d: merge keys (<<) aren't supported", k.Line)
			}
			if _, dup := m[k.Value]; dup {
				return nil, fmt.Errorf("line %d: key %q appears twice", k.Line, k.Value)
			}
			v, err := Value(val)
			if err != nil {
				return nil, err
			}
			m[k.Value] = v
		}
		return m, nil
	}
	return nil, fmt.Errorf("line %d: unsupported YAML node", n.Line)
}

// FromJSON converts a JSON value to a block-style YAML node, keeping key order.
func FromJSON(data []byte) (*yaml.Node, error) {
	if !json.Valid(data) {
		return nil, errors.New("not valid JSON")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("empty JSON value")
	}
	n := doc.Content[0]
	blockStyle(n)
	return n, nil
}

// blockStyle drops the flow and quoting styles JSON input comes with; the
// encoder still quotes strings that would otherwise read as another type.
func blockStyle(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		blockStyle(c)
	}
}

// Encode renders a node as YAML indented by two spaces.
func Encode(n *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
