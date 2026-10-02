package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/diagnose"
	"github.com/j75689/anyship/spec"
)

func (a *app) diagnoseCommand() *cobra.Command {
	var config, target, note, model, effort string
	var showContext, noChecks, yes, asJSON bool
	cmd := &cobra.Command{
		Use:   "diagnose",
		Short: "Ask Claude why a deployment fails, and how to fix anyship.json",
		Long: `Collect what anyship knows about this spec on a target (plan findings,
the target's dry-run checks, status, recent logs and the generated files),
redact secrets, and ask Claude for the root cause and a fix.

A proposed change to anyship.json is shown only if it applies and the patched
spec plans cleanly, and it is written only when you confirm.

Needs Claude API credentials: ANTHROPIC_API_KEY, or ` + "`ant auth login`" + `.
--show-context prints exactly what would be sent, without calling the API.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.prepare(config, target, false)
			if err != nil {
				return err
			}
			raw, err := os.ReadFile(config)
			if err != nil {
				return err
			}

			progress := func(step string) {
				if !asJSON || showContext {
					a.logToStderr("%s...", step)
				}
			}
			collected := diagnose.Collect(cmd.Context(), diagnose.Inputs{
				Spec:     d.spec,
				SpecPath: config,
				SpecRaw:  raw,
				Adapter:  d.adapter,
				NewEnv:   capturingEnv(d.env),
				Note:     note,
				Checks:   !noChecks,
				Progress: progress,
				Redactor: diagnose.NewRedactor(d.spec, os.LookupEnv),
			})
			contextText := collected.Render()
			if showContext {
				_, err := io.WriteString(a.out, contextText)
				return err
			}

			progress(fmt.Sprintf("asking Claude (%s)", orDefault(model, diagnose.DefaultModel)))
			newModel := a.newModel
			if newModel == nil {
				newModel = func(model, effort string) diagnose.Model { return diagnose.NewClaude(model, effort) }
			}
			validate := func(patched []byte) error { return a.validatePatched(cmd.Context(), d, patched) }
			result, err := diagnose.Run(cmd.Context(), newModel(model, effort), contextText, raw, validate)
			if err != nil {
				return err
			}

			if asJSON {
				return printJSON(a.out, diagnoseJSON(result))
			}
			printDiagnosis(a.out, a.style, result, raw)
			if result.Patched == nil {
				return nil
			}
			if !yes {
				ok, err := confirm(a.out, fmt.Sprintf("\nApply this change to %s?", config))
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(a.out, "Left anyship.json unchanged.")
					return nil
				}
			}
			info, err := os.Stat(config)
			if err != nil {
				return err
			}
			if err := os.WriteFile(config, result.Patched, info.Mode().Perm()); err != nil {
				return err
			}
			fmt.Fprintln(a.out, a.style.green("✔ Updated "+config+"; run `anyship plan` and `anyship apply` to try it."))
			return nil
		},
	}
	addConfigFlag(cmd, &config)
	addTargetFlag(cmd, &target)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&note, "note", "", "what went wrong, in your words (an error message, a symptom)")
	cmd.Flags().BoolVar(&showContext, "show-context", false, "print what would be sent to Claude and stop")
	cmd.Flags().BoolVar(&noChecks, "no-checks", false, "skip the target's dry-run checks")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "apply a validated anyship.json change without asking")
	cmd.Flags().StringVar(&model, "model", "", "Claude model (default "+diagnose.DefaultModel+")")
	cmd.Flags().StringVar(&effort, "effort", "", "low, medium, high, xhigh or max (default "+diagnose.DefaultEffort+")")
	return cmd
}

// validatePatched accepts a patched spec only if it parses and plans for the
// target without errors.
func (a *app) validatePatched(ctx context.Context, d *deployment, patched []byte) error {
	s, err := spec.Parse(patched)
	if err != nil {
		if problems := problemsOf(err); len(problems) > 0 {
			return &diagnose.ValidationError{Problems: problems}
		}
		return err
	}
	var discard bytes.Buffer
	p, err := d.adapter.Plan(ctx, s, capturingEnv(d.env)(false, &discard))
	if err != nil {
		return err
	}
	var problems []string
	for _, f := range p.Findings {
		if f.Level == adapter.Error {
			problems = append(problems, fmt.Sprintf("plan error %s: %s", f.Code, f.Message))
		}
	}
	if len(problems) > 0 {
		return &diagnose.ValidationError{Problems: problems}
	}
	return nil
}

// capturingEnv derives Envs whose progress and command output go to out and
// whose commands get an empty stdin.
func capturingEnv(base *adapter.Env) func(dryRun bool, out io.Writer) *adapter.Env {
	return func(dryRun bool, out io.Writer) *adapter.Env {
		env := *base
		env.DryRun = dryRun
		env.Logf = func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
		env.Exec = func(ctx context.Context, opts adapter.ExecOptions, name string, args ...string) error {
			return runWith(ctx, opts, stdio{in: bytes.NewReader(nil), out: out, err: out}, name, args...)
		}
		return &env
	}
}

func printDiagnosis(w io.Writer, s styler, r *diagnose.Result, raw []byte) {
	d := r.Diagnosis
	fmt.Fprintf(w, "\n%s %s\n", s.bold("Diagnosis:"), d.Summary)
	fmt.Fprintf(w, "%s (%s confidence)\n%s\n", s.bold("\nRoot cause"), d.Confidence, d.RootCause)
	if len(d.Evidence) > 0 {
		fmt.Fprintln(w, s.bold("\nEvidence"))
		for _, e := range d.Evidence {
			fmt.Fprintf(w, "  • %s\n", e)
		}
	}
	if len(d.Steps) > 0 {
		fmt.Fprintln(w, s.bold("\nNext steps"))
		for i, step := range d.Steps {
			fmt.Fprintf(w, "  %d. %s\n", i+1, step)
		}
	}
	if strings.TrimSpace(d.CodeChange) != "" {
		fmt.Fprintf(w, "%s\n%s\n", s.bold("\nOutside anyship.json"), d.CodeChange)
	}
	switch {
	case r.Patched != nil:
		fmt.Fprintln(w, s.bold("\nProposed change to anyship.json")+s.dim(" (applies cleanly and plans without errors)"))
		for _, line := range diagnose.DescribePatch(raw, d.SpecPatch) {
			fmt.Fprintf(w, "  %s\n", line)
		}
	case r.PatchProblem != "":
		fmt.Fprintln(w, s.yellow(fmt.Sprintf("\nClaude proposed a change to anyship.json, but it didn't validate after %d attempts: %s", diagnose.MaxAttempts, r.PatchProblem)))
	}
}

func diagnoseJSON(r *diagnose.Result) any {
	out := struct {
		Diagnosis    *diagnose.Diagnosis `json:"diagnosis"`
		ValidPatch   bool                `json:"valid_patch"`
		PatchProblem string              `json:"patch_problem,omitempty"`
		PatchedSpec  string              `json:"patched_spec,omitempty"`
	}{Diagnosis: r.Diagnosis, ValidPatch: r.Patched != nil, PatchProblem: r.PatchProblem, PatchedSpec: string(r.Patched)}
	if out.Diagnosis.Evidence == nil {
		out.Diagnosis.Evidence = []string{}
	}
	if out.Diagnosis.Steps == nil {
		out.Diagnosis.Steps = []string{}
	}
	if out.Diagnosis.SpecPatch == nil {
		out.Diagnosis.SpecPatch = []diagnose.PatchOp{}
	}
	return out
}
