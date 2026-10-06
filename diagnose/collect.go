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

	c.add(fmt.Sprintf("anyship.yaml (%s), deploying to the %s target", filepath.Base(in.SpecPath), target), string(in.SpecRaw))
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
	// user:password@ in URLs. The user may be empty (redis://:pw@host), and
	// an unescaped @ inside the password must not end the match early.
	{regexp.MustCompile(`(\b[a-z][a-z0-9+.-]*://[^:/\s@]*:)[^@\s]+@(?:[^@\s/]*@)*`), "${1}[redacted]@"},
}

// secretKey matches a key that names a credential. Patterns that use it are
// case-insensitive.
const secretKey = `[A-Z0-9_.-]*(?:KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|PRIVATE)[A-Z0-9_.-]*`

// blockHeader is a YAML block scalar indicator with nothing else on the line.
const blockHeader = `[|>][+-]?[0-9]?[+-]?[ \t\r]*(?:#[^\n]*)?`

var (
	// SECRET: | followed by indented lines. Group 1 is what precedes the key.
	secretBlock = regexp.MustCompile(`(?i)^([ \t]*(?:-[ \t]+)?)"?` + secretKey + `"?[ \t]*:[ \t]*` + blockHeader + `$`)

	// A secret key that opens a line: API_KEY: value, - API_KEY=value,
	// export API_KEY=value, or the same behind a compose log prefix
	// ("web-1  | "). The value runs to the end of the line, spaces included.
	// Arrays and objects under such keys (like "secrets": [...]) are
	// structure, values already redacted start with "[", and quoted values
	// are left to secretPair.
	secretLine = regexp.MustCompile(`(?im)^([ \t]*(?:[\w.-]+[ \t]*\|[ \t]*)?(?:-[ \t]+)?(?:export[ \t]+|declare[ \t]+-x[ \t]+)?"?` + secretKey + `"?[ \t]*[:=][ \t]*)([^\s,}\[{"'][^\n]*?)(,?[ \t\r]*)$`)

	// A secret key anywhere else, such as password=... inside a connection
	// string: the value ends at whitespace, so its neighbours stay readable.
	// Group 2 is a block scalar header, which is not a value.
	secretPair = regexp.MustCompile(`(?im)("?` + secretKey + `"?[ \t]*[:=][ \t]*)(?:(` + blockHeader + `$)|"[^"\n\[{][^"\n]*"|'[^'\n]*'|[^\s,}\[{"'][^\s,}]*)`)

	// Authorization: Bearer ..., in headers, curl commands and JSON. The
	// value ends at a quote or the end of the line.
	authHeader = regexp.MustCompile(`(?i)(\b(?:proxy-)?authorization"?[ \t]*[:=][ \t]*["']?)([^\s"'\[{][^\n"']*)`)
	authScheme = regexp.MustCompile(`(?i)^(?:basic|bearer|digest|negotiate|token)[ \t]+`)

	blockHeaderOnly = regexp.MustCompile(`^` + blockHeader + `$`)

	// A boolean or null under a secret-looking key is a setting, such as
	// targets.gcp.private, and the model needs to see it.
	plainLiteral = regexp.MustCompile(`(?i)^(?:true|false|null|~)[ \t\r]*(?:#.*)?$`)
)

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
	text = redactSecretBlocks(text)
	text = authHeader.ReplaceAllStringFunc(text, func(match string) string {
		m := authHeader.FindStringSubmatch(match)
		// Keep the scheme: "Bearer" or "Basic" says how the app authenticates.
		scheme := authScheme.FindString(m[2])
		if scheme == m[2] {
			scheme = ""
		}
		if strings.HasPrefix(m[2][len(scheme):], "[") {
			return match // already redacted
		}
		return m[1] + scheme + "[redacted]"
	})
	text = secretLine.ReplaceAllStringFunc(text, func(match string) string {
		m := secretLine.FindStringSubmatch(match)
		if blockHeaderOnly.MatchString(m[2]) || plainLiteral.MatchString(m[2]) {
			return match
		}
		return m[1] + `"[redacted]"` + m[3]
	})
	text = secretPair.ReplaceAllStringFunc(text, func(match string) string {
		m := secretPair.FindStringSubmatch(match)
		if m[2] != "" || plainLiteral.MatchString(match[len(m[1]):]) {
			return match
		}
		return m[1] + `"[redacted]"`
	})
	return text
}

// redactSecretBlocks replaces the lines of a YAML block scalar under a secret
// key with one redacted line. The block is every following line indented
// deeper than the key.
func redactSecretBlocks(text string) string {
	if !strings.ContainsAny(text, "|>") {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		m := secretBlock.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		first, last := 0, i
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			if indentOf(lines[j]) <= len(m[1]) {
				break
			}
			if last == i {
				first = j
			}
			last = j
		}
		if last == i {
			continue
		}
		out = append(out, lines[first][:indentOf(lines[first])]+"[redacted]")
		i = last
	}
	return strings.Join(out, "\n")
}

func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
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
