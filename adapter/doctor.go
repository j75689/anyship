package adapter

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/j75689/anyship/spec"
)

// Tool is one command-line tool a target drives on this machine, as Doctor
// found it.
type Tool struct {
	Name string `json:"name"`
	// Need says what the target uses it for.
	Need    string `json:"need"`
	Found   bool   `json:"found"`
	Version string `json:"version,omitempty"`
	// Min is the oldest version anyship works with, when it needs a
	// feature that older ones lack.
	Min string `json:"min,omitempty"`
	// Login is who the tool is logged in as, or the context it points at,
	// for tools that have one.
	Login string `json:"login,omitempty"`
}

// Checkup is what Doctor reports: the tools, and a finding for each one
// that is missing or not logged in.
type Checkup struct {
	Tools    []Tool
	Findings []Finding
}

// Doctor is implemented by adapters that can check this machine for a
// target: which of the tools it drives are installed, their versions and
// their logins. It runs only those tools, never a deploy or a change, and
// works without a spec; with one (s may be nil) it reads the target's
// options, such as a gcloud configuration or an aws profile.
type Doctor interface {
	Doctor(ctx context.Context, s *spec.Spec, env *Env) Checkup
}

// Probe runs a command with an empty stdin and returns its trimmed stdout
// and stderr, so nothing reaches the terminal or waits on it.
func Probe(ctx context.Context, env *Env, opts ExecOptions, name string, args ...string) (string, string, error) {
	var out, errOut bytes.Buffer
	opts.Dir = env.Dir
	opts.Stdin, opts.Stdout, opts.Stderr = strings.NewReader(""), &out, &errOut
	err := env.Exec(ctx, opts, name, args...)
	return strings.TrimSpace(out.String()), strings.TrimSpace(errOut.String()), err
}

// Missing is the finding for a tool whose version command failed: missing
// itself when the tool isn't installed, or the same code saying it doesn't
// run when it is.
func Missing(missing Finding, tool string, err error) Finding {
	if !NotInstalled(err) {
		missing.Message = fmt.Sprintf("%s is installed but doesn't run: %v.", tool, err)
	}
	return missing
}

// Floor is the oldest version of a tool that has a feature anyship uses.
type Floor struct {
	Min string
	// Feature is what anyship needs that older versions lack.
	Feature string
	// Hint says how to get a newer version.
	Hint string
}

// Check warns under code when version is older than the floor. It is a
// warning, not an error: a version string anyship misreads must not block a
// deploy. An unknown version passes.
func (f Floor) Check(tool, version, code string) []Finding {
	if !Older(version, f.Min) {
		return nil
	}
	return []Finding{{Level: Warning, Code: code, Hint: f.Hint,
		Message: fmt.Sprintf("anyship needs %s %s or newer for %s; this machine has %s.", tool, f.Min, f.Feature, version)}}
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

var versionRe = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

// VersionIn picks the first version number out of a tool's output.
func VersionIn(text string) string { return versionRe.FindString(text) }

// BuildsFromSource reports whether a container target would build an image
// for some service of s, which takes Docker with buildx on this machine.
func BuildsFromSource(s *spec.Spec) bool {
	if s == nil {
		return false
	}
	for _, svc := range s.Services {
		if svc.Image == "" {
			return true
		}
	}
	return false
}

// buildxFloor: images are built with `docker buildx build --metadata-file`,
// whose digest anyship deploys by.
var buildxFloor = Floor{Min: "0.6.0", Feature: "`--metadata-file`", Hint: "Update Docker Desktop, or the buildx plugin: https://github.com/docker/buildx#installing"}

// Buildx checks for docker buildx, which the container targets build with,
// and its version. Codes start with prefix, such as GCP: a missing buildx
// is <prefix>_PREFLIGHT_DOCKER, an error when needed (a service builds from
// source) and a warning otherwise; an old one is <prefix>_PREFLIGHT_VERSION.
func Buildx(ctx context.Context, env *Env, needed bool, prefix string) (Tool, []Finding) {
	tool := Tool{Name: "docker buildx", Need: "builds images from source", Min: buildxFloor.Min}
	out, _, err := Probe(ctx, env, ExecOptions{}, "docker", "buildx", "version")
	if err == nil {
		tool.Found, tool.Version = true, VersionIn(out)
		return tool, buildxFloor.Check(tool.Name, tool.Version, prefix+"_PREFLIGHT_VERSION")
	}
	code, hint := prefix+"_PREFLIGHT_DOCKER", "Install Docker Desktop or the buildx plugin, or set services.<name>.image."
	if needed {
		return tool, []Finding{{Level: Error, Code: code, Message: "Building from source needs Docker with buildx on this machine.", Hint: hint}}
	}
	return tool, []Finding{{Level: Warning, Code: code, Message: "Docker with buildx isn't on this machine; only services built from source need it.", Hint: hint}}
}
