package yamljson

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type shape struct {
	Name     string                     `json:"name"`
	Replicas int                        `json:"replicas"`
	Ratio    float64                    `json:"ratio"`
	Private  *bool                      `json:"private"`
	Secrets  map[string]*inner          `json:"secrets"`
	Ports    []inner                    `json:"ports"`
	Env      map[string]string          `json:"env"`
	Tags     []string                   `json:"tags"`
	Check    *inner                     `json:"healthCheck"`
	Targets  map[string]json.RawMessage `json:"targets"`
	Anything any                        `json:"anything"`
	Skipped  string                     `json:"-"`
}

type inner struct {
	Port int `json:"port"`
}

func problemsOf(t *testing.T, doc string) Problems {
	t.Helper()
	js, err := ToJSON([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return Check(js, reflect.TypeFor[shape]())
}

// strictError is what the decoder every spec and target block goes through
// says about the same document.
func strictError(t *testing.T, doc string) error {
	t.Helper()
	js, err := ToJSON([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	return dec.Decode(&shape{})
}

func TestCheckNamesThePathAndBothShapes(t *testing.T) {
	for doc, want := range map[string]string{
		"secrets: [JWT]":                 "secrets: must be a mapping (key: value), not a list",
		"ports: {port: 80}":              "ports: must be a list (- item), not a mapping",
		"tags: web":                      "tags: must be a list (- item), not a string",
		"healthCheck: /health":           "healthCheck: must be a mapping (key: value), not a string",
		"replicas: two":                  "replicas: must be a whole number, not a string",
		"replicas: 1.5":                  "replicas: must be a whole number, not the number 1.5",
		"ratio: half":                    "ratio: must be a number, not a string",
		"private: yes please":            "private: must be true or false, not a string",
		"private: 1":                     "private: must be true or false, not the number 1",
		"name: 5":                        `name: must be a string, not the number 5; write it in quotes: "5"`,
		"name: true":                     `name: must be a string, not true; write it in quotes: "true"`,
		"name: [a]":                      "name: must be a string, not a list",
		"env: {PORT: 8080, HOST: web}":   `env.PORT: must be a string, not the number 8080; write it in quotes: "8080"`,
		"env: {DEBUG: false}":            `env.DEBUG: must be a string, not false; write it in quotes: "false"`,
		"ports: [{port: 80}, {port: x}]": "ports.1.port: must be a whole number, not a string",
		"secrets: {JWT: [a]}":            "secrets.JWT: must be a mapping (key: value), not a list",
		"- a\n- b":                       "the document must be a mapping (key: value), not a list",
		"replica: 2":                     `replica: unknown field; did you mean "replicas"?`,
		"ports: [{prot: 80}]":            `ports.0.prot: unknown field; did you mean "port"?`,
		"secrets: {JWT: {size: 1}}":      "secrets.JWT.size: unknown field; known fields are port",
		"skipped: x":                     "skipped: unknown field; known fields are anything, env, healthCheck, name, ports, private, ratio, replicas, secrets, tags, targets",
	} {
		got := problemsOf(t, doc)
		if len(got) != 1 || got[0].String() != want {
			t.Errorf("%s:\n got  %v\n want %s", doc, got, want)
		}
		for _, p := range got {
			for _, leak := range []string{"json", "Go ", "unmarshal", "map[", "yamljson", "struct"} {
				if strings.Contains(p.String(), leak) {
					t.Errorf("%s: %q leaks %q", doc, p, leak)
				}
			}
		}
		// Check must not be stricter or laxer than the decoder it speaks for.
		if strictError(t, doc) == nil {
			t.Errorf("%s: Check reports %v, but the strict decoder accepts it", doc, got)
		}
	}
}

func TestCheckFindsEverythingAtOnce(t *testing.T) {
	got := problemsOf(t, "name: 5\nreplica: 2\nports:\n  - port: x\n  - prot: 1\nenv: [a]\n")
	var lines []string
	for _, p := range got {
		lines = append(lines, p.String())
	}
	want := []string{
		"env: must be a mapping (key: value), not a list",
		`name: must be a string, not the number 5; write it in quotes: "5"`,
		"ports.0.port: must be a whole number, not a string",
		`ports.1.prot: unknown field; did you mean "port"?`,
		`replica: unknown field; did you mean "replicas"?`,
	}
	if !slices.Equal(lines, want) {
		t.Errorf("got:\n  %s\nwant:\n  %s", strings.Join(lines, "\n  "), strings.Join(want, "\n  "))
	}
	if msg := got.Error(); !strings.HasPrefix(msg, "env must be a mapping (key: value), not a list; name must be a string") ||
		!strings.Contains(msg, `unknown field "ports.1.prot"; did you mean "port"?`) {
		t.Errorf("as one error: %s", msg)
	}
}

// What the strict decoder accepts, Check accepts.
func TestCheckAcceptsWhatTheDecoderAccepts(t *testing.T) {
	for _, doc := range []string{
		"name: web\nreplicas: 2\nratio: 0.5\nprivate: true",
		"ratio: 2",
		"replicas: -1",
		"name:\nports:\nsecrets:\nhealthCheck:",        // nulls
		"secrets: {JWT: }",                             // a null entry
		"HEALTHCHECK: {port: 1}\nReplicas: 3",          // encoding/json ignores case
		"targets: {vps: {host: [not, checked, here]}}", // a target block is its adapter's to check
		"anything: [1, {a: b}]",
		"env: {}\ntags: []\nports: [{port: 80}, {}]",
	} {
		if got := problemsOf(t, doc); len(got) != 0 {
			t.Errorf("%q: unexpected problems %v", doc, got)
		}
		if err := strictError(t, doc); err != nil {
			t.Errorf("%q: the strict decoder rejects it: %v", doc, err)
		}
	}
}
