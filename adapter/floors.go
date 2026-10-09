package adapter

import (
	"fmt"
	"strconv"
	"strings"
)

// The version floors: the oldest version of each tool that has a feature
// anyship uses, and only where it uses one. Each was taken from the tool's
// own changelog. Keep them here, and list a new one in Floors; the README
// names every floor, which a test checks.
var (
	// KubectlFloor: deploys prune what the spec dropped with
	// --prune-allowlist, which kubectl 1.26 renamed from --prune-whitelist.
	KubectlFloor = Floor{Tool: "kubectl", Min: "1.26", Feature: "`apply --prune-allowlist`",
		Hint: "Install a newer kubectl: https://kubernetes.io/docs/tasks/tools/"}
	// BuildxFloor: images are built with `docker buildx build
	// --metadata-file`, whose digest anyship deploys by; buildx 0.6.0 added it.
	BuildxFloor = Floor{Tool: "docker buildx", Min: "0.6.0", Feature: "`--metadata-file`",
		Hint: "Update Docker Desktop, or the buildx plugin: https://github.com/docker/buildx#installing"}
	// AWSFloor: aws services are ECS Express Mode services, whose
	// *-express-gateway-service commands came with aws CLI 2.32.2.
	AWSFloor = Floor{Tool: "aws CLI", Min: "2.32.2", Feature: "the ECS Express Mode commands (`aws ecs create-express-gateway-service`)",
		Hint: "Update the aws CLI: https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html"}
	// WranglerFloor: anyship renders wrangler.jsonc, which wrangler reads
	// without --experimental-json-config since 3.91.0. Node.js has no floor:
	// each wrangler checks the Node.js it runs on, and says so.
	WranglerFloor = Floor{Tool: "wrangler", Min: "3.91.0", Feature: "`wrangler.jsonc`",
		Hint: "Update the project's wrangler: `npm install --save-dev wrangler@latest`."}
)

// Floors lists every floor, for documentation and its test.
var Floors = []Floor{KubectlFloor, BuildxFloor, AWSFloor, WranglerFloor}

// Floor is the oldest version of a tool that has a feature anyship uses.
type Floor struct {
	// Tool names the tool in messages.
	Tool string
	Min  string
	// Feature is what anyship needs that older versions lack.
	Feature string
	// Hint says how to get a newer version.
	Hint string
}

// Check warns under code when version is older than the floor. It is a
// warning, not an error: a version string anyship misreads must not block a
// deploy. An unknown version passes.
func (f Floor) Check(version, code string) []Finding {
	if !Older(version, f.Min) {
		return nil
	}
	return []Finding{{Level: Warning, Code: code, Hint: f.Hint,
		Message: fmt.Sprintf("anyship needs %s %s or newer for %s; this machine has %s.", f.Tool, f.Min, f.Feature, version)}}
}

// Older reports whether version is older than min, comparing dotted
// numbers ("1.24.3" < "1.26"); a missing part counts as 0. It is false when
// either can't be read.
func Older(version, min string) bool {
	a, okA := parseVersion(version)
	b, okB := parseVersion(min)
	if !okA || !okB {
		return false
	}
	for i := range max(len(a), len(b)) {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func parseVersion(v string) ([]int, bool) {
	v = VersionIn(v)
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}
