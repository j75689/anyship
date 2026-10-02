package diagnose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

const (
	// maxSection keeps the tail of long sections (logs, command output).
	maxSection = 24 << 10
	// logWindow bounds log reads on targets that only stream live logs.
	logWindow = 15 * time.Second
	logTail   = 200
)

// Section is one titled part of the context sent to the model.
type Section struct {
	Title string
	Body  string
}

// Context is the redacted deployment context, in the order it was collected.
type Context struct {
	Sections []Section
	redact   *Redactor
}

func (c *Context) add(title, body string) {
	body = strings.TrimSpace(c.redact.Redact(body))
	if len(body) > maxSection {
		body = "[... earlier output cut ...]\n" + body[len(body)-maxSection:]
	}
	c.Sections = append(c.Sections, Section{Title: title, Body: body})
}

// Render formats the context as Markdown.
func (c *Context) Render() string {
	var b strings.Builder
	for _, s := range c.Sections {
		body := s.Body
		if body == "" {
			body = "(empty)"
		}
		fmt.Fprintf(&b, "## %s\n\n```\n%s\n```\n\n", s.Title, strings.ReplaceAll(body, "```", "'''"))
	}
	return b.String()
}

// Inputs is what Collect needs to gather context for one spec and target.
type Inputs struct {
	Spec     *spec.Spec
	SpecPath string
	SpecRaw  []byte
	Adapter  adapter.Adapter
	// NewEnv returns an Env for the target whose logs and command output go to out.
	NewEnv func(dryRun bool, out io.Writer) *adapter.Env
	// Note is what the user saw or wants checked, in their words.
	Note string
	// Checks runs the target's dry run (such as the vps preflight checks).
	Checks bool
	// Progress reports each step; may be nil.
	Progress func(step string)
	Redactor *Redactor
}

// Collect gathers the deployment context. Failures to collect a part are
// recorded in the context rather than returned, since they are often the
// clue.
func Collect(ctx context.Context, in Inputs) *Context {
	c := &Context{redact: in.Redactor}
	progress := func(step string) {
		if in.Progress != nil {
			in.Progress(step)
		}
	}
	target := in.Adapter.Name()

	c.add(fmt.Sprintf("anyship.json (%s), deploying to the %s target", filepath.Base(in.SpecPath), target), string(in.SpecRaw))
	if in.Note != "" {
		c.add("What the user reports", in.Note)
	}

	progress("planning")
	var out bytes.Buffer
	plan, err := in.Adapter.Plan(ctx, in.Spec, in.NewEnv(false, &out))
	if err != nil {
		c.add("anyship plan", "plan failed: "+err.Error()+"\n"+out.String())
		return c
	}
	c.add("anyship plan: findings and actions", toJSON(map[string]any{"findings": nonNil(plan.Findings), "actions": nonNil(plan.Actions)}))
	for _, f := range plan.Files {
		c.add("Generated file "+filepath.Base(f.Path), string(f.Contents))
	}

	if in.Checks && !adapter.HasErrors(plan.Findings) {
		progress("running the target's checks (dry run)")
		out.Reset()
		result, err := in.Adapter.Apply(ctx, plan, in.Spec, in.NewEnv(true, &out))
		switch {
		case err != nil:
			c.add("Target checks (dry run)", "failed: "+err.Error()+"\n"+out.String())
		default:
			c.add("Target checks (dry run)", toJSON(map[string]any{"ok": result.OK, "findings": nonNil(result.Findings), "messages": result.Messages})+"\n\nOutput:\n"+out.String())
		}
	}

	if reader, ok := in.Adapter.(adapter.StatusReader); ok {
		progress("reading status")
		out.Reset()
		st, err := reader.Status(ctx, in.Spec, in.NewEnv(false, &out))
		if err != nil {
			c.add("Status", "failed: "+err.Error()+"\n"+out.String())
		} else {
			c.add("Status", toJSON(st))
		}
	}

	if reader, ok := in.Adapter.(adapter.LogReader); ok {
		progress(fmt.Sprintf("reading logs (up to %s for live streams)", logWindow))
		out.Reset()
		window, cancel := context.WithTimeout(ctx, logWindow)
		err := reader.Logs(window, in.Spec, in.NewEnv(false, &out), adapter.LogOptions{Tail: logTail})
		cancel()
		body := out.String()
		if err != nil && window.Err() == nil {
			body = "failed: " + err.Error() + "\n" + body
		}
		c.add(fmt.Sprintf("Recent logs (last %d lines per service)", logTail), body)
	}
	return c
}

// Redactor removes secrets from text before it leaves the machine.
type Redactor struct {
	values []secretValue
}

type secretValue struct{ value, name string }

var redactPatterns = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), "[redacted private key]"},
	{regexp.MustCompile(`\b(?:sk-ant-[A-Za-z0-9_-]{16,}|sk-[A-Za-z0-9_-]{20,}|ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})\b`), "[redacted token]"},
	// user:password@ in URLs
	{regexp.MustCompile(`(\b[a-z][a-z0-9+.-]*://[^:/\s@]+:)[^@\s]+@`), "${1}[redacted]@"},
	// "API_KEY": "value", API_KEY=value and similar. Only scalar values: arrays
	// and objects under such keys (like "secrets": [...]) are structure, and
	// values already redacted start with "[".
	{regexp.MustCompile(`(?i)("?[A-Z0-9_.-]*(?:KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|PRIVATE)[A-Z0-9_.-]*"?\s*[:=]\s*)("[^"\n\[{][^"\n]*"|'[^'\n]*'|[^\s,}\[{"'][^\s,}]*)`), `${1}"[redacted]"`},
}

// NewRedactor redacts the values of the spec's secrets that are set in the
// deployer's environment, plus anything that looks like a credential.
func NewRedactor(s *spec.Spec, lookupEnv func(string) (string, bool)) *Redactor {
	r := &Redactor{}
	for name := range s.Secrets {
		if v, ok := lookupEnv(name); ok && len(v) >= 4 {
			r.values = append(r.values, secretValue{value: v, name: name})
		}
	}
	// Longer values first, so one secret containing another is fully replaced.
	slices.SortFunc(r.values, func(a, b secretValue) int { return len(b.value) - len(a.value) })
	return r
}

func (r *Redactor) Redact(text string) string {
	if r == nil {
		return text
	}
	for _, s := range r.values {
		text = strings.ReplaceAll(text, s.value, "[redacted secret "+s.name+"]")
	}
	for _, p := range redactPatterns {
		text = p.re.ReplaceAllString(text, p.with)
	}
	return text
}

func toJSON(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(data)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
