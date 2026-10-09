package adapter_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
)

func TestOlder(t *testing.T) {
	for _, tc := range []struct {
		version, min string
		want         bool
	}{
		{"1.24.3", "1.26", true}, {"1.26", "1.26", false}, {"1.26.0", "1.26", false}, {"1.33.9", "1.26", false},
		{"2.31.40", "2.32.2", true}, {"2.32.10", "2.32.2", false}, {"3.114.0", "3.91.0", false}, {"3.90.9", "3.91.0", true},
		{"", "1.26", false}, {"unknown", "1.26", false},
	} {
		if got := adapter.Older(tc.version, tc.min); got != tc.want {
			t.Errorf("Older(%q, %q) = %v", tc.version, tc.min, got)
		}
	}
	floor := adapter.Floor{Tool: "kubectl", Min: "1.26", Feature: "x"}
	if floor.Check("", "C") != nil || floor.Check("1.30.1", "C") != nil {
		t.Error("a newer or unknown version was warned about")
	}
	if got := floor.Check("1.25.0", "C"); len(got) != 1 || got[0].Level != adapter.Warning ||
		got[0].Message != "anyship needs kubectl 1.26 or newer for x; this machine has 1.25.0." {
		t.Errorf("old version: %+v", got)
	}
}

// The README's table of floors is written by hand; it must list each one.
func TestREADMEListsEveryFloor(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range adapter.Floors {
		if row := fmt.Sprintf("| %s | %s |", f.Tool, f.Min); !strings.Contains(string(readme), row) {
			t.Errorf("README.md has no row %q", row)
		}
	}
}
