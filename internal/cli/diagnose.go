package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/diagnose"
)

func (a *app) diagnoseCommand() *cobra.Command {
	var config, target, note string
	var noChecks, asJSON bool
	cmd := &cobra.Command{
		Use:   "diagnose",
		Short: "Collect what anyship knows about a failing deployment, for your agent to diagnose",
		Long: `Collect what anyship knows about this spec on a target (plan findings,
the target's dry-run checks, status, recent logs and the generated files),
redact secrets, and print it as Markdown.

Hand the output to whatever AI agent you use, with your question. With the
MCP server (anyship mcp) the agent fetches the same context itself through
the diagnose_context tool. anyship calls no model and changes nothing; a fix
is an edit to anyship.yaml that anyship plan checks like any other.

Redaction works by pattern: the values of the spec's secrets found in your
environment, credential-like keys, Authorization headers, API tokens,
passwords in URLs and private keys. Read the output before pasting it
anywhere you would not paste a secret.`,
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
				if !asJSON {
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
			if asJSON {
				return printJSON(a.out, struct {
					Context string `json:"context"`
				}{collected.Render()})
			}
			_, err = io.WriteString(a.out, collected.Render())
			return err
		},
	}
	addConfigFlag(cmd, &config)
	addTargetFlag(cmd, &target)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().StringVar(&note, "note", "", "what went wrong, in your words (an error message, a symptom)")
	cmd.Flags().BoolVar(&noChecks, "no-checks", false, "skip the target's dry-run checks")
	return cmd
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
