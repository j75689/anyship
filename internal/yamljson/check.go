package yamljson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Problem is one place where a document does not fit the type it is decoded
// into: a value of the wrong shape, or a field the type does not have.
type Problem struct {
	// Path is the dotted path from the document's root, such as
	// spec.services.web.ports.0.port. It is empty for the root itself.
	Path string
	// Unknown is set for a field the type does not have.
	Unknown bool
	// Detail says what belongs at Path and what was found ("must be a list
	// (- item), not a mapping"), or, for an unknown field, which field was
	// probably meant ("; did you mean \"replicas\"?").
	Detail string
}

// String is the problem as a line in a list of them: "path: what is wrong".
func (p Problem) String() string {
	switch {
	case p.Unknown:
		return p.Path + ": unknown field" + p.Detail
	case p.Path == "":
		return "the document " + p.Detail
	}
	return p.Path + ": " + p.Detail
}

// Sentence is the problem as part of a sentence, for a message that already
// says which block it is about: "port must be a whole number, not a string".
func (p Problem) Sentence() string {
	switch {
	case p.Unknown:
		return fmt.Sprintf("unknown field %q%s", p.Path, p.Detail)
	case p.Path == "":
		return p.Detail
	}
	return p.Path + " " + p.Detail
}

// Problems is every Problem of a document, as one error.
type Problems []Problem

func (p Problems) Error() string {
	parts := make([]string, len(p))
	for i, problem := range p {
		parts[i] = problem.Sentence()
	}
	return strings.Join(parts, "; ")
}

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// Check compares a JSON document, as ToJSON produces it, with the type it
// will be decoded into and returns every place the two disagree, in a stable
// order. It finds what a strict json.Decoder would refuse, but reports all of
// it at once and in the file's terms: encoding/json stops at the first error,
// names Go types ("cannot unmarshal array into Go struct field
// Manifest.spec.secrets of type map[string]*spec.Secret"), and gives no path
// for an unknown field.
//
// Like encoding/json, it matches field names without regard to case, accepts
// null anywhere, and leaves json.RawMessage and interface fields alone.
func Check(js []byte, t reflect.Type) Problems {
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil // not JSON: the caller's decoder says so
	}
	var problems Problems
	check(value, t, nil, &problems)
	return problems
}

func check(value any, t reflect.Type, at []string, problems *Problems) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if value == nil || t == rawMessageType || t.Kind() == reflect.Interface {
		return
	}
	mismatch := func() {
		detail := "must be " + expected(t) + ", not " + found(value)
		// The usual way to get here: YAML reads 80 or true as a number or a
		// boolean where the field holds text.
		if _, isString := value.(string); t.Kind() == reflect.String && !isString {
			if literal, ok := scalar(value); ok {
				detail += fmt.Sprintf(`; write it in quotes: "%s"`, literal)
			}
		}
		*problems = append(*problems, Problem{Path: strings.Join(at, "."), Detail: detail})
	}

	switch t.Kind() {
	case reflect.Struct:
		m, ok := value.(map[string]any)
		if !ok {
			mismatch()
			return
		}
		fields := jsonFields(t)
		names := slices.Sorted(maps.Keys(fields))
		for _, key := range slices.Sorted(maps.Keys(m)) {
			field, known := fields[key]
			if !known {
				field, known = foldedField(fields, key)
			}
			if !known {
				*problems = append(*problems, Problem{Path: strings.Join(sub(at, key), "."), Unknown: true, Detail: hintFor(key, names)})
				continue
			}
			check(m[key], field.Type, sub(at, key), problems)
		}
	case reflect.Map:
		m, ok := value.(map[string]any)
		if !ok {
			mismatch()
			return
		}
		for _, key := range slices.Sorted(maps.Keys(m)) {
			check(m[key], t.Elem(), sub(at, key), problems)
		}
	case reflect.Slice, reflect.Array:
		items, ok := value.([]any)
		if !ok {
			mismatch()
			return
		}
		for i, item := range items {
			check(item, t.Elem(), sub(at, strconv.Itoa(i)), problems)
		}
	case reflect.String:
		if _, ok := value.(string); !ok {
			mismatch()
		}
	case reflect.Bool:
		if _, ok := value.(bool); !ok {
			mismatch()
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n, ok := value.(json.Number); !ok || strings.ContainsAny(n.String(), ".eE") {
			mismatch()
		}
	case reflect.Float32, reflect.Float64:
		if _, ok := value.(json.Number); !ok {
			mismatch()
		}
	}
}

// sub extends a path without writing into the caller's slice.
func sub(at []string, key string) []string { return append(slices.Clip(at), key) }

// expected names the YAML shape a Go type decodes from.
func expected(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Map, reflect.Struct:
		return "a mapping (key: value)"
	case reflect.Slice, reflect.Array:
		return "a list (- item)"
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "true or false"
	case reflect.Float32, reflect.Float64:
		return "a number"
	}
	return "a whole number"
}

// found names what the document holds instead.
func found(value any) string {
	switch v := value.(type) {
	case map[string]any:
		return "a mapping"
	case []any:
		return "a list"
	case string:
		return "a string"
	case json.Number:
		return "the number " + v.String()
	}
	return fmt.Sprint(value) // true or false
}

// scalar returns a number or a boolean the way it is written.
func scalar(value any) (string, bool) {
	switch v := value.(type) {
	case json.Number:
		return v.String(), true
	case bool:
		return strconv.FormatBool(v), true
	}
	return "", false
}

// jsonFields maps a struct's JSON field names to its fields.
func jsonFields(t reflect.Type) map[string]reflect.StructField {
	fields := map[string]reflect.StructField{}
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() || f.Anonymous {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		fields[name] = f
	}
	return fields
}

// foldedField finds a field whose name differs from key only in case, which
// encoding/json accepts.
func foldedField(fields map[string]reflect.StructField, key string) (reflect.StructField, bool) {
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if strings.EqualFold(name, key) {
			return fields[name], true
		}
	}
	return reflect.StructField{}, false
}

// hintFor suggests the field that was probably meant, or lists the ones that
// exist when nothing is close.
func hintFor(key string, known []string) string {
	if nearest := nearest(key, known); nearest != "" {
		return fmt.Sprintf("; did you mean %q?", nearest)
	}
	return "; known fields are " + strings.Join(known, ", ")
}

// nearest returns the known field within two edits of key, ignoring case, or
// "" when none is that close.
func nearest(key string, known []string) string {
	best, bestDistance := "", min(3, len(key))
	for _, candidate := range known {
		if d := distance(strings.ToLower(key), strings.ToLower(candidate)); d < bestDistance {
			best, bestDistance = candidate, d
		}
	}
	return best
}

// distance is the Levenshtein distance between a and b.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}
