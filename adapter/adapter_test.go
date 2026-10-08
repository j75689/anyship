package adapter

import "testing"

func TestOptionsHint(t *testing.T) {
	got := OptionsHint("vps", "`host: deploy@203.0.113.10`, an ssh destination")
	want := "Under spec.targets.vps in anyship.yaml, set `host: deploy@203.0.113.10`, an ssh destination. " +
		"Every option: `anyship targets vps`."
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestDecodeOptionsSpeaksYAML(t *testing.T) {
	type options struct {
		Host string `json:"host"`
		Port int    `json:"port,omitempty"`
		Sudo bool   `json:"sudo,omitempty"`
	}
	for raw, want := range map[string]string{
		`{"host": "h", "port": "22"}`:         "port must be a whole number, not a string",
		`{"host": 5}`:                         `host must be a string, not the number 5; write it in quotes: "5"`,
		`{"host": "h", "sudo": "yes"}`:        "sudo must be true or false, not a string",
		`{"host": "h", "prot": 22}`:           `unknown field "prot"; did you mean "port"?`,
		`{"host": "h", "zone": "a"}`:          `unknown field "zone"; known fields are host, port, sudo`,
		`["h"]`:                               "must be a mapping (key: value), not a list",
		`{"host": 5, "port": "x", "zone": 1}`: `host must be a string, not the number 5; write it in quotes: "5"; port must be a whole number, not a string; unknown field "zone"; known fields are host, port, sudo`,
	} {
		err := DecodeOptions([]byte(raw), &options{})
		if err == nil || err.Error() != want {
			t.Errorf("%s:\n got  %v\n want %s", raw, err, want)
		}
	}
	var o options
	if err := DecodeOptions([]byte(`{"host": "deploy@h", "port": 2222, "sudo": true}`), &o); err != nil || o != (options{"deploy@h", 2222, true}) {
		t.Errorf("valid options: %+v %v", o, err)
	}
}
