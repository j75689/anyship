package diagnose

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// PatchOp is one RFC 6902 operation on anyship.json. Only add, replace and
// remove are supported; Value carries the JSON-encoded value.
type PatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

// ApplyPatch applies ops to a JSON document, keeping the document's key order
// so a change touches only what it changes. The result is indented with two
// spaces and ends with a newline.
func ApplyPatch(doc []byte, ops []PatchOp) ([]byte, error) {
	root, err := decodeOrdered(doc)
	if err != nil {
		return nil, fmt.Errorf("anyship.json is not valid JSON: %w", err)
	}
	for i, op := range ops {
		if root, err = applyOp(root, op); err != nil {
			return nil, fmt.Errorf("patch operation %d (%s %s): %w", i+1, op.Op, op.Path, err)
		}
	}
	var buf bytes.Buffer
	encodeOrdered(&buf, root, "")
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// DescribePatch renders ops for people, showing old values next to new ones.
func DescribePatch(doc []byte, ops []PatchOp) []string {
	root, _ := decodeOrdered(doc)
	var lines []string
	for _, op := range ops {
		old, found := lookup(root, op.Path)
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

func applyOp(root any, op PatchOp) (any, error) {
	tokens, err := splitPointer(op.Path)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, errors.New("replacing the whole document is not allowed")
	}
	var value any
	if op.Op == "add" || op.Op == "replace" {
		if value, err = decodeOrdered([]byte(op.Value)); err != nil {
			return nil, fmt.Errorf("value is not valid JSON: %w", err)
		}
	} else if op.Op != "remove" {
		return nil, fmt.Errorf("unsupported op %q (use add, replace or remove)", op.Op)
	}

	parent, err := walk(root, tokens[:len(tokens)-1])
	if err != nil {
		return nil, err
	}
	last := tokens[len(tokens)-1]
	switch p := parent.(type) {
	case *object:
		_, exists := p.values[last]
		switch {
		case op.Op == "add":
			p.set(last, value)
		case !exists:
			return nil, fmt.Errorf("%q does not exist", last)
		case op.Op == "replace":
			p.set(last, value)
		default:
			p.remove(last)
		}
		return root, nil
	case *array:
		if op.Op == "add" && last == "-" {
			p.items = append(p.items, value)
			return root, nil
		}
		i, err := strconv.Atoi(last)
		if err != nil || i < 0 || i > len(p.items) || (op.Op != "add" && i == len(p.items)) {
			return nil, fmt.Errorf("array index %q is out of range", last)
		}
		switch op.Op {
		case "add":
			p.items = append(p.items[:i], append([]any{value}, p.items[i:]...)...)
		case "replace":
			p.items[i] = value
		default:
			p.items = append(p.items[:i], p.items[i+1:]...)
		}
		return root, nil
	default:
		return nil, errors.New("parent is not an object or array")
	}
}

func lookup(root any, path string) (any, bool) {
	tokens, err := splitPointer(path)
	if err != nil {
		return nil, false
	}
	v, err := walk(root, tokens)
	return v, err == nil
}

func walk(v any, tokens []string) (any, error) {
	for _, t := range tokens {
		switch n := v.(type) {
		case *object:
			next, ok := n.values[t]
			if !ok {
				return nil, fmt.Errorf("%q does not exist", t)
			}
			v = next
		case *array:
			i, err := strconv.Atoi(t)
			if err != nil || i < 0 || i >= len(n.items) {
				return nil, fmt.Errorf("array index %q is out of range", t)
			}
			v = n.items[i]
		default:
			return nil, fmt.Errorf("cannot index into a scalar with %q", t)
		}
	}
	return v, nil
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

// object is a JSON object that remembers its key order.
type object struct {
	keys   []string
	values map[string]any
}

func (o *object) set(key string, v any) {
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = v
}

func (o *object) remove(key string) {
	delete(o.values, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			return
		}
	}
}

type array struct{ items []any }

func decodeOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &object{values: map[string]any{}}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				o.set(keyTok.(string), v)
			}
			_, err := dec.Token() // }
			return o, err
		case '[':
			a := &array{items: []any{}}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				a.items = append(a.items, v)
			}
			_, err := dec.Token() // ]
			return a, err
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default:
		return t, nil // string, json.Number, bool or nil
	}
}

func encodeOrdered(buf *bytes.Buffer, v any, indent string) {
	inner := indent + "  "
	switch n := v.(type) {
	case *object:
		if len(n.keys) == 0 {
			buf.WriteString("{}")
			return
		}
		buf.WriteString("{\n")
		for i, k := range n.keys {
			buf.WriteString(inner)
			writeScalar(buf, k)
			buf.WriteString(": ")
			encodeOrdered(buf, n.values[k], inner)
			if i < len(n.keys)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		buf.WriteString(indent + "}")
	case *array:
		if len(n.items) == 0 {
			buf.WriteString("[]")
			return
		}
		buf.WriteString("[\n")
		for i, item := range n.items {
			buf.WriteString(inner)
			encodeOrdered(buf, item, inner)
			if i < len(n.items)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		buf.WriteString(indent + "]")
	default:
		writeScalar(buf, n)
	}
}

func writeScalar(buf *bytes.Buffer, v any) {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // scalars always encode
	buf.Truncate(buf.Len() - 1)
}

func compact(v any) string {
	var buf bytes.Buffer
	encodeOrdered(&buf, v, "")
	var out bytes.Buffer
	if json.Compact(&out, buf.Bytes()) != nil {
		return buf.String()
	}
	return out.String()
}

func short(s string) string {
	const limit = 120
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
