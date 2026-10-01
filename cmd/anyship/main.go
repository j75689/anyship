// Command anyship deploys any app to any platform from one spec.
package main

import (
	"os"

	"github.com/j75689/anyship/internal/cli"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(cli.Execute(version))
}
