// Package adapter defines the contract between anyship and a deploy target.
package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/j75689/anyship/spec"
)

type Level string

const (
	// Error blocks apply.
	Error   Level = "error"
	Warning Level = "warning"
	Info    Level = "info"
)

// Finding is something an adapter or the detector wants the user to know.
type Finding struct {
	Level Level `json:"level"`
	// Code is a stable identifier such as CF_VOLUMES, safe to match on in tests and docs.
	Code    string `json:"code"`
	Message string `json:"message"`
	Service string `json:"service,omitempty"`
	File    string `json:"file,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

// HasErrors reports whether any finding blocks apply.
func HasErrors(findings []Finding) bool {
	return slices.ContainsFunc(findings, func(f Finding) bool { return f.Level == Error })
}

type Op string

const (
	OpCreate Op = "create"
	OpRun    Op = "run"
	OpDeploy Op = "deploy"
	OpNote   Op = "note"
)

type Action struct {
	Op Op `json:"op"`
	// Kind is what is acted on, e.g. "worker", "D1", "build".
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
}

type File struct {
	// Path is absolute.
	Path     string `json:"path"`
	Contents []byte `json:"-"`
}

type Plan struct {
	Target   string    `json:"target"`
	Findings []Finding `json:"findings"`
	Actions  []Action  `json:"actions"`
	Files    []File    `json:"files"`
	// Data is adapter-private state handed from Plan to Apply.
	Data any `json:"-"`
}

type Result struct {
	OK       bool
	Messages []string
}

type ExecOptions struct {
	Dir string
	// Shell runs the command through the system shell (for user-supplied build commands).
	Shell bool
	// Stdin, when set, is streamed to the command instead of the terminal's stdin.
	Stdin io.Reader
	// Stdout, when set, captures the command's output instead of showing it.
	Stdout io.Writer
}

// ExitCode returns the exit status carried by an error from Env.Exec.
func ExitCode(err error) (int, bool) {
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) {
		return exit.ExitCode(), true
	}
	return 0, false
}

// Env is what an adapter may use to touch the outside world.
type Env struct {
	// Dir is the directory containing anyship.json.
	Dir string
	// OutDir is scratch space for generated platform config, e.g. <Dir>/.anyship/<target>.
	OutDir string
	DryRun bool
	Logf   func(format string, args ...any)
	// Exec runs a command with inherited stdio (unless opts.Stdin is set) and
	// returns an error if it fails.
	Exec func(ctx context.Context, opts ExecOptions, name string, args ...string) error
	// LookupEnv reads the deployer's environment, e.g. for secret values.
	LookupEnv func(key string) (string, bool)
}

// Adapter is a deploy target. Plan must be side-effect free: it only inspects
// the spec and reports what Apply would do. Apply must refuse a plan with errors.
type Adapter interface {
	Name() string
	Description() string
	Plan(ctx context.Context, s *spec.Spec, env *Env) (*Plan, error)
	Apply(ctx context.Context, p *Plan, s *spec.Spec, env *Env) (*Result, error)
}

// LogOptions selects which runtime logs to show.
type LogOptions struct {
	// Service limits output to one service; empty means all of them.
	Service string
	Follow  bool
	// Tail is the number of recent lines per service; 0 means the adapter's default.
	Tail int
	// Since is a duration such as "10m" or an RFC 3339 / YYYY-MM-DD timestamp.
	Since      string
	Timestamps bool
}

// LogReader is implemented by adapters that can show a deployment's runtime
// logs. Logs streams to the terminal and returns when done, or when ctx is
// cancelled while following. Options the platform can't honor are errors.
type LogReader interface {
	Logs(ctx context.Context, s *spec.Spec, env *Env, opts LogOptions) error
}

// Status is what is currently running for a spec on a target.
type Status struct {
	Target string `json:"target"`
	// Location says where the deployment lives, e.g. "deploy@host:anyship/app".
	Location string          `json:"location"`
	Deployed bool            `json:"deployed"`
	Services []ServiceStatus `json:"services"`
}

type ServiceStatus struct {
	Name string `json:"name"`
	// State is "running" when every instance runs, "missing" when none
	// exist, or the platform's state of the first instance that doesn't run
	// (such as "exited" or "restarting").
	State string `json:"state"`
	// Health is "healthy", "unhealthy" or "starting"; empty without a health check.
	Health  string   `json:"health,omitempty"`
	Running int      `json:"running"`
	Desired int      `json:"desired"`
	Ports   []string `json:"ports,omitempty"`
	// Detail is the platform's own description, e.g. "Up 2 hours".
	Detail string `json:"detail,omitempty"`
}

// Healthy reports whether every service runs as desired and none is unhealthy.
func (s *Status) Healthy() bool {
	if !s.Deployed {
		return false
	}
	for _, svc := range s.Services {
		if svc.Running < svc.Desired || svc.Health == "unhealthy" {
			return false
		}
	}
	return true
}

// StatusReader is implemented by adapters that can report what is running.
type StatusReader interface {
	Status(ctx context.Context, s *spec.Spec, env *Env) (*Status, error)
}

type DestroyOptions struct {
	// Volumes also deletes persistent data: volumes, secrets and deployment files.
	Volumes bool
}

// Destroyer is implemented by adapters that can remove a deployment.
type Destroyer interface {
	// DestroySummary describes what Destroy would remove, for confirmation.
	// It must not touch the target.
	DestroySummary(s *spec.Spec, opts DestroyOptions) ([]string, error)
	Destroy(ctx context.Context, s *spec.Spec, env *Env, opts DestroyOptions) (*Result, error)
}

// Registry holds the adapters available to the CLI.
type Registry struct {
	adapters map[string]Adapter
}

func NewRegistry(adapters ...Adapter) (*Registry, error) {
	r := &Registry{adapters: map[string]Adapter{}}
	for _, a := range adapters {
		if _, dup := r.adapters[a.Name()]; dup {
			return nil, fmt.Errorf("adapter %q is registered twice", a.Name())
		}
		r.adapters[a.Name()] = a
	}
	return r, nil
}

func (r *Registry) Get(name string) (Adapter, error) {
	if a, ok := r.adapters[name]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("unknown target %q; available: %s", name, strings.Join(r.Names(), ", "))
}

func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.adapters))
	for name := range r.adapters {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (r *Registry) List() []Adapter {
	var out []Adapter
	for _, name := range r.Names() {
		out = append(out, r.adapters[name])
	}
	return out
}
