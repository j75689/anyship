package cli

import "testing"

func TestValidSince(t *testing.T) {
	for s, want := range map[string]bool{
		"10m":                  true,
		"1h30m":                true,
		"2026-10-01":           true,
		"2026-10-01T12:00:00Z": true,
		"0s":                   false,
		"-5m":                  false,
		"yesterday":            false,
		"10m; rm -rf /":        false,
	} {
		if got := validSince(s); got != want {
			t.Errorf("validSince(%q) = %v, want %v", s, got, want)
		}
	}
}
