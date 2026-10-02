package spec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// legacyVersion is the anyship.json format version Migrate converts. Older or
// newer numbers are refused rather than guessed at.
const legacyVersion = 1

// legacyKeys are the top-level keys a version 1 anyship.json could have. The
// first three became the manifest envelope; the rest moved under spec as they
// were.
var legacyKeys = []string{"$schema", "version", "name", "services", "resources", "secrets", "targets"}

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// Migrate converts a version 1 anyship.json into anyship.yaml. Only the
// envelope changed, so the conversion is mechanical: version and name become
// apiVersion, kind and metadata.name, and services, resources, secrets and
// targets move under spec unchanged. Anything Migrate doesn't recognize is
// reported, never dropped: a field the old spec deployed with must not go
// missing without the user hearing about it.
//
// The result is a well-formed manifest, but not necessarily a valid one: a
// spec that was already broken migrates into a broken spec. Parse the result
// to find out.
func Migrate(legacy []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(legacy))
	var root map[string]json.RawMessage
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("not a valid %s: %w", LegacyFilename, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("not a valid %s: unexpected data after the top-level object", LegacyFilename)
	}
	if _, ok := root["apiVersion"]; ok {
		return nil, fmt.Errorf("this is already an %s manifest, not a version %d %s; rename it to %s",
			APIVersion, legacyVersion, LegacyFilename, Filename)
	}

	var p problems
	if err := checkLegacyVersion(root["version"]); err != nil {
		p = append(p, err.Error())
	}
	for _, key := range slices.Sorted(maps.Keys(root)) {
		if !slices.Contains(legacyKeys, key) {
			p.add([]any{key}, "unknown field%s", hintFor(key, legacyKeys))
		}
	}

	body := map[string]json.RawMessage{}
	for _, key := range []string{"services", "resources", "secrets", "targets"} {
		if raw, ok := root[key]; ok {
			body[key] = raw
		}
	}
	appendUnknownFields(body, &p)
	if len(p) > 0 {
		return nil, &ValidationError{Problems: p}
	}

	s, err := decodeLegacySpec(body)
	if err != nil {
		return nil, err
	}
	if raw, ok := root["name"]; ok {
		if err := json.Unmarshal(raw, &s.Name); err != nil {
			return nil, fmt.Errorf("not a valid %s: name: %w", LegacyFilename, err)
		}
	}
	return Marshal(s)
}

func checkLegacyVersion(raw json.RawMessage) error {
	if raw == nil {
		return fmt.Errorf("version: is required; `anyship migrate` only converts version %d specs, written by anyship v0.2.x", legacyVersion)
	}
	var version int
	if err := json.Unmarshal(raw, &version); err != nil || version != legacyVersion {
		return fmt.Errorf("version: must be %d; `anyship migrate` only converts specs written by anyship v0.2.x", legacyVersion)
	}
	return nil
}

// decodeLegacySpec decodes the part of anyship.json that moves under spec.
// Unknown fields are already reported with their path, so this only has to
// catch values of the wrong type.
func decodeLegacySpec(body map[string]json.RawMessage) (*Spec, error) {
	js, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("not a valid %s: %w", LegacyFilename, err)
	}
	return &s, nil
}

// appendUnknownFields reports every field of body that Spec has no place for,
// at its full path, so the user can decide what to do with it.
func appendUnknownFields(body map[string]json.RawMessage, p *problems) {
	var value any
	js, err := json.Marshal(body)
	if err != nil || json.Unmarshal(js, &value) != nil {
		return
	}
	unknownFields(value, reflect.TypeFor[Spec](), []any{"spec"}, p)
}

func unknownFields(value any, t reflect.Type, at []any, p *problems) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == rawMessageType {
		return // a per-target block: its adapter decodes and validates it
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := value.(map[string]any)
		if !ok {
			return // the wrong type here; the decoder reports it
		}
		fields := jsonFields(t)
		for _, key := range slices.Sorted(maps.Keys(m)) {
			field, known := fields[key]
			if !known {
				p.add(sub(at, key), "unknown field%s", hintFor(key, slices.Sorted(maps.Keys(fields))))
				continue
			}
			unknownFields(m[key], field.Type, sub(at, key), p)
		}
	case reflect.Map:
		m, ok := value.(map[string]any)
		if !ok {
			return
		}
		for _, key := range slices.Sorted(maps.Keys(m)) {
			unknownFields(m[key], t.Elem(), sub(at, key), p)
		}
	case reflect.Slice:
		items, ok := value.([]any)
		if !ok {
			return
		}
		for i, item := range items {
			unknownFields(item, t.Elem(), sub(at, i), p)
		}
	}
}

// sub extends a path without writing into the caller's slice.
func sub(at []any, key any) []any { return append(slices.Clip(at), key) }

// jsonFields maps a struct's JSON field names to their fields.
func jsonFields(t reflect.Type) map[string]reflect.StructField {
	fields := map[string]reflect.StructField{}
	for _, f := range reflect.VisibleFields(t) {
		if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
			fields[name] = f
		}
	}
	return fields
}

// hintFor suggests the field the user probably meant, or lists the ones that
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
