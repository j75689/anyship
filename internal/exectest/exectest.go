// Package exectest fakes adapter.Env.Exec for tests: commands are answered
// by the longest matching prefix of their command line.
package exectest

import (
	"context"
	"io"
	"os/exec"
	"strings"

	"github.com/j75689/anyship/adapter"
)

// Answer is what a fake command prints and returns.
type Answer struct {
	Stdout, Stderr string
	Err            error
}

// Missing is the answer of a command that isn't installed.
func Missing(name string) Answer { return Answer{Err: &exec.Error{Name: name, Err: exec.ErrNotFound}} }

// Fake answers commands from Answers, by the longest prefix of the command
// line that has an answer; a command no prefix matches succeeds silently.
// Calls records each command line with its options.
type Fake struct {
	Answers map[string]Answer
	Calls   []Call
}

type Call struct {
	Line string
	Opts adapter.ExecOptions
}

func (f *Fake) Exec(_ context.Context, opts adapter.ExecOptions, name string, args ...string) error {
	line := strings.Join(append([]string{name}, args...), " ")
	f.Calls = append(f.Calls, Call{Line: line, Opts: opts})
	var best string
	for prefix := range f.Answers {
		if strings.HasPrefix(line, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	a, ok := f.Answers[best]
	if !ok {
		return nil
	}
	if opts.Stdout != nil {
		_, _ = io.WriteString(opts.Stdout, a.Stdout)
	}
	if opts.Stderr != nil {
		_, _ = io.WriteString(opts.Stderr, a.Stderr)
	}
	return a.Err
}

// Env returns an Env whose commands f answers.
func (f *Fake) Env(dir string) *adapter.Env {
	return &adapter.Env{Dir: dir, Logf: func(string, ...any) {}, Exec: f.Exec, LookupEnv: func(string) (string, bool) { return "", false }}
}

// Codes lists the findings' codes.
func Codes(findings []adapter.Finding) []string {
	codes := []string{}
	for _, f := range findings {
		codes = append(codes, f.Code)
	}
	return codes
}
