// Package adapter defines the contract between anyship and a deploy target.
package adapter

import (
	"context"
	"fmt"
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
}

// Env is what an adapter may use to touch the outside world.
type Env struct {
	// Dir is the directory containing anyship.json.
	Dir string
	// OutDir is scratch space for generated platform config, e.g. <Dir>/.anyship/<target>.
	OutDir string
	DryRun bool
	Logf   func(format string, args ...any)
	// Exec runs a command with inherited stdio and returns an error if it fails.
	Exec func(ctx context.Context, opts ExecOptions, name string, args ...string) error
}

// Adapter is a deploy target. Plan must be side-effect free: it only inspects
// the spec and reports what Apply would do. Apply must refuse a plan with errors.
type Adapter interface {
	Name() string
	Description() string
	Plan(ctx context.Context, s *spec.Spec, env *Env) (*Plan, error)
	Apply(ctx context.Context, p *Plan, s *spec.Spec, env *Env) (*Result, error)
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
