// Package anyship holds what the binary carries from the repository: the
// pages that describe each target, served to agents over MCP as resources
// so that a hint about an option has somewhere to point that isn't a URL.
package anyship

import "embed"

// TargetDocs holds docs/targets/<name>.md for every target that has a page.
//
//go:embed docs/targets/*.md
var TargetDocs embed.FS
