package cli

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/detect"
	"github.com/j75689/anyship/spec"
)

// The commands' --json output and the MCP tools' results are the same types,
// built by the same functions, so a script and an agent read the same thing
// whichever way they reach anyship. Only the wording differs: see audience.

// audience is who reads a message. Messages are written for someone at a
// shell; an agent that reached anyship over MCP has tools instead, so its
// audience rewrites the commands a message names into those tools.
type audience struct {
	text func(string) string
	err  func(error) error
}

var (
	shell = audience{text: func(s string) string { return s }, err: hintForShell}
	agent = audience{text: agentText, err: forAgent}
)

func (a audience) lines(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = a.text(line)
	}
	return out
}

func (a audience) findings(findings []adapter.Finding) []adapter.Finding {
	out := make([]adapter.Finding, len(findings))
	for i, f := range findings {
		f.Message, f.Hint = a.text(f.Message), a.text(f.Hint)
		out[i] = f
	}
	return out
}

// hintForShell adds what to run next to an error that has an obvious fix.
func hintForShell(err error) error {
	var missing *spec.NotFoundError
	if errors.As(err, &missing) {
		return fmt.Errorf("%w; run `anyship init` in the project to draft it", err)
	}
	return err
}

type targetsOutput struct {
	Targets []targetInfo `json:"targets"`
}

type targetInfo struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
	// Docs says how to read the target's page, when it has one: the MCP
	// resource, or the command that prints it.
	Docs string `json:"docs,omitempty"`
}

// targetPage is one target with its page, for `anyship targets <name> --json`.
type targetPage struct {
	targetInfo
	Page string `json:"page"`
}

// newTargetsOutput lists the targets; docs names how to read a target's page.
func newTargetsOutput(registry *adapter.Registry, docs func(name string) string) targetsOutput {
	out := targetsOutput{Targets: []targetInfo{}}
	for _, ad := range registry.List() {
		out.Targets = append(out.Targets, newTargetInfo(ad, docs))
	}
	return out
}

func newTargetInfo(ad adapter.Adapter, docs func(name string) string) targetInfo {
	info := targetInfo{Name: ad.Name(), Description: ad.Description(), Capabilities: capabilities(ad)}
	if _, err := targetDoc(ad.Name()); err == nil {
		info.Docs = docs(ad.Name())
	}
	return info
}

// capabilities lists the commands a target supports.
func capabilities(ad adapter.Adapter) []string {
	caps := []string{"plan", "apply"}
	if _, ok := ad.(adapter.LogReader); ok {
		caps = append(caps, "logs")
	}
	if _, ok := ad.(adapter.StatusReader); ok {
		caps = append(caps, "status")
	}
	if _, ok := ad.(adapter.Destroyer); ok {
		caps = append(caps, "destroy")
	}
	return caps
}

type detectOutput struct {
	// Dir is the directory that was inspected, as an absolute path: where
	// the draft belongs.
	Dir string `json:"dir"`
	// Spec is the drafted anyship.yaml, to review and write to the project.
	Spec     string            `json:"spec"`
	Valid    bool              `json:"valid"`
	Problems []string          `json:"problems"`
	Findings []adapter.Finding `json:"findings"`
	Evidence []string          `json:"evidence"`
	// Existing describes the anyship.yaml the directory already has, if
	// any: the one to validate and plan, rather than replace with the draft.
	Existing *existingSpec `json:"existing,omitempty"`
}

// existingSpec is what detect says about an anyship.yaml already in place.
type existingSpec struct {
	Path     string   `json:"path"`
	Valid    bool     `json:"valid"`
	Problems []string `json:"problems"`
	Name     string   `json:"name,omitempty"`
	Services []string `json:"services"`
	// Targets are the targets the spec configures under spec.targets.
	Targets []string `json:"targets"`
}

// newDetectOutput drafts a spec for dir and writes nothing.
func newDetectOutput(dir string, aud audience) (detectOutput, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return detectOutput{}, err
	}
	d, err := detect.Project(dir)
	if err != nil {
		return detectOutput{}, err
	}
	data, err := spec.Marshal(d.Spec)
	if err != nil {
		return detectOutput{}, err
	}
	out := detectOutput{Dir: dir, Spec: string(data), Valid: true, Problems: []string{}, Findings: aud.findings(d.Findings), Evidence: aud.lines(d.Evidence)}
	if _, err := spec.Parse(data); err != nil {
		out.Valid, out.Problems = false, aud.lines(problemsOrError(err))
	}
	if raw, err := os.ReadFile(filepath.Join(dir, spec.Filename)); err == nil {
		ex := &existingSpec{Path: filepath.Join(dir, spec.Filename), Problems: []string{}, Services: []string{}, Targets: []string{}}
		if s, err := spec.Parse(raw); err != nil {
			ex.Problems = problemsOrError(aud.err(err))
		} else {
			ex.Valid, ex.Name, ex.Services, ex.Targets = true, s.Name, s.ServiceNames(), targetNames(s)
		}
		out.Existing = ex
	}
	return out, nil
}

type validateOutput struct {
	Valid    bool     `json:"valid"`
	Problems []string `json:"problems"`
	Name     string   `json:"name,omitempty"`
	Services []string `json:"services"`
	// Targets are the targets the spec configures under spec.targets, the
	// ones to plan for.
	Targets []string `json:"targets"`
}

// newValidateOutput checks the spec at config. A missing or unreadable file
// is a problem too, not an error: the caller asked whether the spec is good.
func newValidateOutput(config string, aud audience) validateOutput {
	s, err := spec.Load(config)
	if err != nil {
		return validateOutput{Problems: problemsOrError(aud.err(err)), Services: []string{}, Targets: []string{}}
	}
	return validateOutput{Valid: true, Problems: []string{}, Name: s.Name, Services: s.ServiceNames(), Targets: targetNames(s)}
}

type planOutput struct {
	Target   string            `json:"target"`
	Ready    bool              `json:"ready"`
	Findings []adapter.Finding `json:"findings"`
	Actions  []adapter.Action  `json:"actions"`
	Files    []planFile        `json:"files"`
}

// planFile is a file the target would generate. plan writes nothing, so the
// contents come with it: Path holds nothing on disk until a deploy writes it.
type planFile struct {
	Path     string `json:"path"`
	Contents string `json:"contents"`
}

// newPlanOutput is a plan with empty lists kept as lists and the generated
// files' contents, since plan writes none of them.
func newPlanOutput(p *adapter.Plan, aud audience) planOutput {
	out := planOutput{Target: p.Target, Ready: !adapter.HasErrors(p.Findings), Findings: aud.findings(p.Findings), Actions: nonNil(p.Actions), Files: []planFile{}}
	for _, f := range p.Files {
		out.Files = append(out.Files, planFile{Path: f.Path, Contents: string(f.Contents)})
	}
	return out
}

// statusOutput is the target's status with the verdict the CLI's exit code
// gives: deployed, every service running as desired and none unhealthy.
type statusOutput struct {
	adapter.Status
	Healthy bool `json:"healthy"`
}

func newStatusOutput(st *adapter.Status) statusOutput {
	out := statusOutput{Status: *st, Healthy: st.Healthy()}
	out.Services = nonNil(out.Services)
	return out
}

type resultOutput struct {
	OK       bool              `json:"ok"`
	Summary  []string          `json:"summary,omitempty"`
	Findings []adapter.Finding `json:"findings"`
	Messages []string          `json:"messages"`
	// Output is what the target's tools (ssh, wrangler, ...) printed.
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
	// Files are the generated files apply wrote under .anyship/<target>/.
	Files []string `json:"files,omitempty"`
}

// newResultOutput is the outcome of an apply or a destroy, with the tail of
// what the target's tools printed.
func newResultOutput(r *adapter.Result, summary []string, output *tailBuffer, aud audience) resultOutput {
	out := resultOutput{OK: r.OK, Summary: aud.lines(summary), Findings: aud.findings(r.Findings), Messages: aud.lines(r.Messages)}
	out.Output, out.Truncated = output.String()
	return out
}

// planErrorsOutput is the outcome when the plan has errors and nothing was done.
func planErrorsOutput(p *adapter.Plan, aud audience) resultOutput {
	return resultOutput{Findings: aud.findings(p.Findings), Messages: []string{"The plan has errors; nothing was done. Fix anyship.yaml and plan again."}}
}

// dryRunDestroyOutput is the outcome of a destroy dry run: the summary, and
// nothing removed.
func dryRunDestroyOutput(summary []string, aud audience) resultOutput {
	return resultOutput{OK: true, Summary: aud.lines(summary), Findings: []adapter.Finding{}, Messages: []string{"Dry run: nothing was removed."}}
}

// targetNames lists the targets a spec configures, sorted; [] when none, so
// JSON readers see a list either way.
func targetNames(s *spec.Spec) []string {
	return nonNil(slices.Sorted(maps.Keys(s.Targets)))
}

func problemsOrError(err error) []string {
	if problems := problemsOf(err); len(problems) > 0 {
		return problems
	}
	return []string{err.Error()}
}

func problemsOf(err error) []string {
	var verr *spec.ValidationError
	if errors.As(err, &verr) {
		return verr.Problems
	}
	return nil
}

// nonNil keeps empty lists as [] in JSON output.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
