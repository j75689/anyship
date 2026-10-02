package diagnose

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/j75689/anyship/internal/yamljson"
)

// PatchOp is one RFC 6902 operation on anyship.yaml, addressed as if the
// document were JSON. Only add, replace and remove are supported; Value
// carries the JSON-encoded value.
type PatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

// ApplyPatch applies ops to a YAML document in place, so key order and
// comments outside the changed values survive. The result is indented with
// two spaces.
func ApplyPatch(doc []byte, ops []PatchOp) ([]byte, error) {
	root, err := yamljson.Parse(doc)
	if err != nil {
		return nil, fmt.Errorf("anyship.yaml is not valid YAML: %w", err)
	}
	if len(root.Content) == 0 {
		return nil, errors.New("anyship.yaml is empty")
	}
	for i, op := range ops {
		if err := applyOp(root.Content[0], op); err != nil {
			return nil, fmt.Errorf("patch operation %d (%s %s): %w", i+1, op.Op, op.Path, err)
		}
	}
	return yamljson.Encode(root)
}

// DescribePatch renders ops for people, showing old values next to new ones.
func DescribePatch(doc []byte, ops []PatchOp) []string {
	var top *yaml.Node
	if root, err := yamljson.Parse(doc); err == nil && len(root.Content) > 0 {
		top = root.Content[0]
	}
	var lines []string
	for _, op := range ops {
		old, found := lookup(top, op.Path)
		switch {
		case op.Op == "remove" && found:
			lines = append(lines, fmt.Sprintf("remove %s (was %s)", op.Path, short(compact(old))))
		case op.Op == "replace" && found:
			lines = append(lines, fmt.Sprintf("replace %s: %s → %s", op.Path, short(compact(old)), short(op.Value)))
		default:
			lines = append(lines, fmt.Sprintf("%s %s: %s", op.Op, op.Path, short(op.Value)))
		}
	}
	return lines
}

func applyOp(root *yaml.Node, op PatchOp) error {
	tokens, err := splitPointer(op.Path)
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return errors.New("replacing the whole document is not allowed")
	}
	var value *yaml.Node
	if op.Op == "add" || op.Op == "replace" {
		if value, err = yamljson.FromJSON([]byte(op.Value)); err != nil {
			return fmt.Errorf("value is not valid JSON: %w", err)
		}
	} else if op.Op != "remove" {
		return fmt.Errorf("unsupported op %q (use add, replace or remove)", op.Op)
	}

	parent, err := walk(root, tokens[:len(tokens)-1])
	if err != nil {
		return err
	}
	last := tokens[len(tokens)-1]
	switch parent.Kind {
	case yaml.MappingNode:
		i := keyIndex(parent, last)
		switch {
		case i < 0 && op.Op == "add":
			key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: last}
			parent.Content = append(parent.Content, key, value)
		case i < 0:
			return fmt.Errorf("%q does not exist", last)
		case op.Op == "remove":
			parent.Content = append(parent.Content[:i], parent.Content[i+2:]...)
		default:
			old := parent.Content[i+1]
			value.LineComment, value.FootComment = old.LineComment, old.FootComment
			parent.Content[i+1] = value
		}
		return nil
	case yaml.SequenceNode:
		items := parent.Content
		if op.Op == "add" && last == "-" {
			parent.Content = append(items, value)
			return nil
		}
		i, err := strconv.Atoi(last)
		if err != nil || i < 0 || i > len(items) || (op.Op != "add" && i == len(items)) {
			return fmt.Errorf("array index %q is out of range", last)
		}
		switch op.Op {
		case "add":
			parent.Content = append(items[:i], append([]*yaml.Node{value}, items[i:]...)...)
		case "replace":
			items[i] = value
		default:
			parent.Content = append(items[:i], items[i+1:]...)
		}
		return nil
	default:
		return errors.New("parent is not an object or array")
	}
}

// keyIndex returns the index of key's key node in a mapping, or -1.
func keyIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func lookup(root *yaml.Node, path string) (*yaml.Node, bool) {
	if root == nil {
		return nil, false
	}
	tokens, err := splitPointer(path)
	if err != nil {
		return nil, false
	}
	n, err := walk(root, tokens)
	return n, err == nil
}

func walk(n *yaml.Node, tokens []string) (*yaml.Node, error) {
	for _, t := range tokens {
		switch n.Kind {
		case yaml.MappingNode:
			i := keyIndex(n, t)
			if i < 0 {
				return nil, fmt.Errorf("%q does not exist", t)
			}
			n = n.Content[i+1]
		case yaml.SequenceNode:
			i, err := strconv.Atoi(t)
			if err != nil || i < 0 || i >= len(n.Content) {
				return nil, fmt.Errorf("array index %q is out of range", t)
			}
			n = n.Content[i]
		case yaml.AliasNode:
			return nil, errors.New("editing through a YAML alias is not supported")
		default:
			return nil, fmt.Errorf("cannot index into a scalar with %q", t)
		}
	}
	return n, nil
}

// splitPointer parses an RFC 6901 JSON Pointer.
func splitPointer(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("path %q must start with /", path)
	}
	tokens := strings.Split(path[1:], "/")
	for i, t := range tokens {
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
	}
	return tokens, nil
}

// compact renders a node as one line of JSON.
func compact(n *yaml.Node) string {
	v, err := yamljson.Value(n)
	if err != nil {
		return n.Value
	}
	out, err := json.Marshal(v)
	if err != nil {
		return n.Value
	}
	return string(out)
}

func short(s string) string {
	const limit = 120
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
