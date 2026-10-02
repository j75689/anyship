package yamljson

import (
	"strings"
	"testing"
)

func TestToJSON(t *testing.T) {
	got, err := ToJSON([]byte(`
# comment
name: web
port: 8080
quoted: "8080"
on: yes
list:
  - a
  - &x {k: 1}
  - *x
`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"list":["a",{"k":1},{"k":1}],"name":"web","on":"yes","port":8080,"quoted":"8080"}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestToJSONRejects(t *testing.T) {
	for name, src := range map[string]string{
		"duplicate key": "a: 1\na: 2\n",
		"merge key":     "base: &b {x: 1}\nother:\n  <<: *b\n",
		"two documents": "a: 1\n---\nb: 2\n",
		"empty":         "",
		"syntax":        "a: [1, 2\n",
	} {
		if _, err := ToJSON([]byte(src)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestFromJSONKeepsOrderAndQuotesAmbiguousStrings(t *testing.T) {
	n, err := FromJSON([]byte(`{"z": 1, "a": {"port": "8080", "flag": "true", "list": ["x"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Encode(n)
	if err != nil {
		t.Fatal(err)
	}
	want := "z: 1\na:\n  port: \"8080\"\n  flag: \"true\"\n  list:\n    - x\n"
	if string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
	if _, err := FromJSON([]byte("a: 1")); err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Errorf("non-JSON input should be refused, got %v", err)
	}
}
