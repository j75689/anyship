package adapter

import "testing"

func TestOptionsHint(t *testing.T) {
	got := OptionsHint("vps", "`host: deploy@203.0.113.10`, an ssh destination")
	want := "Under spec.targets.vps in anyship.yaml, set `host: deploy@203.0.113.10`, an ssh destination. " +
		"Every option: https://github.com/j75689/anyship/blob/main/docs/targets/vps.md"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
