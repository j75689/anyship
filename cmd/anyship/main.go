// Command anyship deploys any app to any platform from one spec.
package main

import (
	"os"
	"runtime/debug"
	"strings"

	"github.com/j75689/anyship/internal/cli"
)

// version is set by release builds with -ldflags "-X main.version=...".
var version string

func main() {
	os.Exit(cli.Execute(resolveVersion()))
}

// resolveVersion prefers the release version, then the module version that
// `go install ...@vX.Y.Z` (or a VCS-stamped build) records, then "dev".
func resolveVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return "dev"
}
