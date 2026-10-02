package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/j75689/anyship/adapter"
)

// styler adds ANSI colors when writing to a terminal that wants them.
type styler struct{ enabled bool }

func newStyler(f *os.File) styler {
	return styler{enabled: os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" && isTerminal(f)}
}

func (s styler) wrap(code, text string) string {
	if !s.enabled {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s styler) red(t string) string    { return s.wrap("31", t) }
func (s styler) yellow(t string) string { return s.wrap("33", t) }
func (s styler) green(t string) string  { return s.wrap("32", t) }
func (s styler) dim(t string) string    { return s.wrap("2", t) }
func (s styler) bold(t string) string   { return s.wrap("1", t) }

func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

func (s styler) findingIcon(level adapter.Level) string {
	switch level {
	case adapter.Error:
		return s.red("✖")
	case adapter.Warning:
		return s.yellow("!")
	default:
		return s.dim("i")
	}
}

func (s styler) actionIcon(op adapter.Op) string {
	switch op {
	case adapter.OpCreate:
		return s.green("+")
	case adapter.OpDeploy:
		return s.green("↑")
	case adapter.OpRun:
		return s.dim("$")
	default:
		return s.dim("•")
	}
}

func printFindings(w io.Writer, s styler, findings []adapter.Finding) {
	for _, f := range findings {
		scope, file := "", ""
		if f.Service != "" {
			scope = s.dim("[" + f.Service + "] ")
		}
		if f.File != "" {
			file = s.dim(" (" + f.File + ")")
		}
		fmt.Fprintf(w, "  %s %s%s%s\n", s.findingIcon(f.Level), scope, f.Message, file)
		if f.Hint != "" {
			fmt.Fprintln(w, s.dim("      → "+f.Hint))
		}
	}
}

// printResult shows what an adapter found on the target, then its outcome.
func printResult(w io.Writer, s styler, r *adapter.Result) {
	if len(r.Findings) > 0 {
		fmt.Fprintln(w, s.bold("\nChecks on the target:"))
		printFindings(w, s, r.Findings)
		fmt.Fprintln(w)
	}
	for _, m := range r.Messages {
		if r.OK {
			fmt.Fprintln(w, s.green("✔ "+m))
		} else {
			fmt.Fprintln(w, s.red("✖ "+m))
		}
	}
}

// printJSON writes v as indented JSON.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// planJSON is the --json form of a plan: empty lists stay lists, and
// generated files are listed by path.
func planJSON(p *adapter.Plan) any {
	type file struct {
		Path string `json:"path"`
	}
	out := struct {
		Target   string            `json:"target"`
		Ready    bool              `json:"ready"`
		Findings []adapter.Finding `json:"findings"`
		Actions  []adapter.Action  `json:"actions"`
		Files    []file            `json:"files"`
	}{
		Target:   p.Target,
		Ready:    !adapter.HasErrors(p.Findings),
		Findings: append([]adapter.Finding{}, p.Findings...),
		Actions:  append([]adapter.Action{}, p.Actions...),
		Files:    []file{},
	}
	for _, f := range p.Files {
		out.Files = append(out.Files, file{Path: f.Path})
	}
	return out
}

func printStatus(w io.Writer, s styler, project string, st *adapter.Status) {
	fmt.Fprintln(w, s.bold(fmt.Sprintf("%s on %s (%s)", project, st.Target, st.Location)))
	if !st.Deployed {
		fmt.Fprintln(w, s.yellow("Not deployed."))
		return
	}
	// No colors inside the table: escape codes would throw off the alignment.
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  SERVICE\tSTATE\tHEALTH\tRUNNING\tPORTS\tDETAIL")
	for _, svc := range st.Services {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%d/%d\t%s\t%s\n",
			svc.Name, svc.State, orDash(svc.Health), svc.Running, svc.Desired, orDash(strings.Join(svc.Ports, ", ")), orDash(svc.Detail))
	}
	_ = tw.Flush()
	if st.Healthy() {
		fmt.Fprintln(w, s.green("All services are running."))
	} else {
		fmt.Fprintln(w, s.yellow("Some services aren't running as desired; `anyship logs` may say why."))
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printPlan(w io.Writer, s styler, p *adapter.Plan, dir string) {
	fmt.Fprintln(w, s.bold("\nPlan for "+p.Target))
	if len(p.Findings) > 0 {
		fmt.Fprintln(w, "\nFindings:")
		printFindings(w, s, p.Findings)
	}
	if len(p.Actions) > 0 {
		fmt.Fprintln(w, "\nActions:")
		for _, a := range p.Actions {
			detail := ""
			if a.Detail != "" {
				detail = s.dim("  " + a.Detail)
			}
			fmt.Fprintf(w, "  %s %s %s%s\n", s.actionIcon(a.Op), a.Kind, s.bold(a.Name), detail)
		}
	}
	if len(p.Files) > 0 {
		fmt.Fprintln(w, "\nGenerated files"+s.dim(" (a deploy writes them)")+":")
		for _, f := range p.Files {
			rel, err := filepath.Rel(dir, f.Path)
			if err != nil {
				rel = f.Path
			}
			fmt.Fprintf(w, "  ~ %s\n", filepath.ToSlash(rel))
		}
	}
	errors := 0
	for _, f := range p.Findings {
		if f.Level == adapter.Error {
			errors++
		}
	}
	if errors > 0 {
		fmt.Fprintf(w, "\n%s — this plan cannot be applied.\n", s.red(fmt.Sprintf("%d error(s)", errors)))
	} else {
		fmt.Fprintln(w, "\n"+s.green("Plan is ready."))
	}
}
