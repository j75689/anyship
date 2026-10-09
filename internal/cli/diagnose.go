package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
			progress := func(step string) {
				if !asJSON {
					a.logToStderr("%s...", step)
				}
			}
			text, err := diagnoseContext(cmd.Context(), d, config, note, !noChecks, progress)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(a.out, diagnoseContextOutput{Context: text})
			}
			_, err = io.WriteString(a.out, text)
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

// diagnoseContext collects the redacted context for d's spec on its target,
// as Markdown. The target's checks are a dry run of apply, which writes the
// files it would deploy, and some targets need them on disk. diagnose only
// reads, so they go to a temporary directory that is removed before this
// returns, and the lines that name it are dropped: the context holds the
// files' contents already.
func diagnoseContext(ctx context.Context, d *deployment, config, note string, checks bool, progress func(string)) (string, error) {
	raw, err := os.ReadFile(config)
	if err != nil {
		return "", err
	}
	outDir, err := os.MkdirTemp("", "anyship-diagnose-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(outDir) }()
	d.env.OutDir = outDir
	collected := diagnose.Collect(ctx, diagnose.Inputs{
		Spec:     d.spec,
		SpecPath: config,
		SpecRaw:  raw,
		Adapter:  d.adapter,
		NewEnv:   capturingEnv(d),
		Note:     note,
		Checks:   checks,
		Progress: progress,
		Redactor: diagnose.NewRedactor(d.spec, os.LookupEnv),
	})
	return withoutLinesAbout(collected.Render(), filepath.Base(outDir)), nil
}

// capturingEnv derives Envs for d's target whose progress and command
// output go to out and whose commands get an empty stdin.
func capturingEnv(d *deployment) func(dryRun bool, out io.Writer) *adapter.Env {
	return func(dryRun bool, out io.Writer) *adapter.Env {
		env := *d.env
		env.DryRun = dryRun
		env.Logf = func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
		env.Exec = execFor(d.adapter.Name(), stdio{in: bytes.NewReader(nil), out: out, err: out})
		return &env
	}
}
