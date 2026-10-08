package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/j75689/anyship/adapter"
)

func (a *app) statusCommand() *cobra.Command {
	var config, target string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show what is running for this spec on a target",
		Long:  "Show what is running for this spec on a target.\n\nExits with status 1 when the spec isn't deployed or a service isn't fully running, so it works in scripts.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.prepare(config, target, false)
			if err != nil {
				return err
			}
			reader, ok := d.adapter.(adapter.StatusReader)
			if !ok {
				return fmt.Errorf("the %s target does not report status yet", d.adapter.Name())
			}
			if asJSON {
				d.env.Logf = a.logToStderr
			}
			st, err := reader.Status(cmd.Context(), d.spec, d.env)
			if err != nil {
				return err
			}
			if asJSON {
				if err := printJSON(a.out, newStatusOutput(st)); err != nil {
					return err
				}
			} else {
				printStatus(a.out, a.style, d.spec.Name, st)
			}
			if !st.Healthy() {
				return errReported
			}
			return nil
		},
	}
	addConfigFlag(cmd, &config)
	addTargetFlag(cmd, &target)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func (a *app) destroyCommand() *cobra.Command {
	var config, target string
	var volumes, yes, dryRun, asJSON bool
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Remove this spec's deployment from a target",
		Long: "Remove this spec's deployment from a target.\n\n" +
			"Persistent data (volumes, secrets, bound databases) is kept unless --volumes is given.\n" +
			"With --json the outcome is printed as JSON; progress and the platform's tools' output go to stderr.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.prepare(config, target, false)
			if err != nil {
				return err
			}
			destroyer, ok := d.adapter.(adapter.Destroyer)
			if !ok {
				return fmt.Errorf("the %s target can't destroy deployments yet", d.adapter.Name())
			}
			var output *tailBuffer
			if asJSON {
				output = a.keepStdoutForJSON(d)
			}
			opts := adapter.DestroyOptions{Volumes: volumes}
			summary, err := destroyer.DestroySummary(d.spec, opts)
			if err != nil {
				return err
			}

			if asJSON && dryRun {
				return printJSON(a.out, dryRunDestroyOutput(summary, shell))
			}
			if !asJSON {
				fmt.Fprintln(a.out, a.style.bold(fmt.Sprintf("Destroy %s on %s:", d.spec.Name, d.adapter.Name())))
				for _, line := range summary {
					if strings.HasPrefix(line, "DELETE") {
						line = a.style.red(line)
					}
					fmt.Fprintf(a.out, "  • %s\n", line)
				}
			}
			if dryRun {
				// Show what actually exists, when the target can say.
				if reader, ok := d.adapter.(adapter.StatusReader); ok {
					st, err := reader.Status(cmd.Context(), d.spec, d.env)
					if err != nil {
						return err
					}
					fmt.Fprintln(a.out)
					printStatus(a.out, a.style, d.spec.Name, st)
				}
				fmt.Fprintln(a.out, a.style.dim("\nDry run: nothing was removed."))
				return nil
			}
			if !yes {
				var confirmed bool
				w := a.promptWriter(asJSON)
				if volumes {
					confirmed, err = confirmTyped(w, fmt.Sprintf("\nThis deletes data that can't be recovered. Type %q to confirm:", d.spec.Name), d.spec.Name)
				} else {
					confirmed, err = confirm(w, "\nDestroy?")
				}
				if err != nil {
					return err
				}
				if !confirmed {
					fmt.Fprintln(w, "Aborted.")
					return nil
				}
			}

			result, err := destroyer.Destroy(cmd.Context(), d.spec, d.env, opts)
			if err != nil {
				return err
			}
			if asJSON {
				if err := printJSON(a.out, newResultOutput(result, summary, output, shell)); err != nil {
					return err
				}
			} else {
				printResult(a.out, a.style, result)
			}
			if !result.OK {
				return errReported
			}
			return nil
		},
	}
	addConfigFlag(cmd, &config)
	addTargetFlag(cmd, &target)
	cmd.Flags().BoolVar(&volumes, "volumes", false, "also delete volumes, secrets and deployment files")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be removed and what is running now, without removing anything")
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// confirmTyped asks the user to type want exactly, for irreversible actions.
func confirmTyped(w io.Writer, prompt, want string) (bool, error) {
	if !isTerminal(os.Stdin) {
		return false, errors.New("refusing to delete data without confirmation in a non-interactive shell; pass --yes")
	}
	fmt.Fprintf(w, "%s ", prompt)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return strings.TrimSpace(answer) == want, nil
}

// logToStderr keeps progress messages out of stdout when it carries JSON.
func (a *app) logToStderr(format string, args ...any) {
	fmt.Fprintln(os.Stderr, newStyler(os.Stderr).dim(fmt.Sprintf(format, args...)))
}
