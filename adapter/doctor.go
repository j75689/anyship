package adapter

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
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
// and stderr, so nothing reaches the terminal or waits on it. It runs in
// env.Dir unless opts.Dir names another directory.
func Probe(ctx context.Context, env *Env, opts ExecOptions, name string, args ...string) (string, string, error) {
	var out, errOut bytes.Buffer
	if opts.Dir == "" {
		opts.Dir = env.Dir
	}
	opts.Stdin, opts.Stdout, opts.Stderr = strings.NewReader(""), &out, &errOut
	err := env.Exec(ctx, opts, name, args...)
	return strings.TrimSpace(out.String()), strings.TrimSpace(errOut.String()), err
}

// Missing is the finding for a tool whose version command failed: missing
// itself when the tool isn't installed, or the same code saying it doesn't
// run, and why, when it is; the install hint gives way to one about that.
func Missing(missing Finding, tool string, err error) Finding {
	if !NotInstalled(err) {
		missing.Message = fmt.Sprintf("%s is installed but doesn't run: %v.", tool, err)
		missing.Hint = fmt.Sprintf("Run `%s` by hand to see why, or reinstall it.", tool)
	}
	return missing
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

// Buildx checks for docker buildx, which the container targets build with,
// and its version. Codes start with prefix, such as GCP: a missing buildx
// is <prefix>_PREFLIGHT_DOCKER, an error when needed (a service builds from
// source) and a warning otherwise; an old one is <prefix>_PREFLIGHT_VERSION.
func Buildx(ctx context.Context, env *Env, needed bool, prefix string) (Tool, []Finding) {
	tool := Tool{Name: "docker buildx", Need: "builds images from source", Min: BuildxFloor.Min}
	out, _, err := Probe(ctx, env, ExecOptions{}, "docker", "buildx", "version")
	if err == nil {
		tool.Found, tool.Version = true, VersionIn(out)
		return tool, BuildxFloor.Check(tool.Version, prefix+"_PREFLIGHT_VERSION")
	}
	code, hint := prefix+"_PREFLIGHT_DOCKER", "Install Docker Desktop or the buildx plugin, or set services.<name>.image."
	if needed {
		return tool, []Finding{{Level: Error, Code: code, Message: "Building from source needs Docker with buildx on this machine.", Hint: hint}}
	}
	return tool, []Finding{{Level: Warning, Code: code, Message: "Docker with buildx isn't on this machine; only services built from source need it.", Hint: hint}}
}
