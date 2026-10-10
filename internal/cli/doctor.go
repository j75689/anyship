package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

func (a *app) doctorCommand() *cobra.Command {
	var config, target string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that this machine has the tools a target deploys with, and their logins",
		Long: `Check the command-line tools each target drives on this machine: whether
they are installed, their versions, and who they are logged in as.

Without --target it checks the targets anyship.yaml names under
spec.targets, or every target when there is no spec or it names none. The
spec's options pick the gcloud configuration, aws profile or kube context
to check. doctor deploys nothing and changes nothing; it runs only the
tools themselves (wrangler's login check asks Cloudflare). It exits 1 when
a check fails, and --json prints the same result for an issue report.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := a.newDoctorOutput(cmd.Context(), config, cmd.Flags().Changed("config"), target, shell)
			if err != nil {
				return err
			}
			if asJSON {
				if err := printJSON(a.out, out); err != nil {
					return err
				}
			} else {
				a.printDoctor(out)
			}
			if !out.OK {
				return errReported
			}
			return nil
		},
	}
	addConfigFlag(cmd, &config)
	cmd.Flags().StringVarP(&target, "target", "t", "", `deploy target to check; defaults to the ones anyship.yaml names`)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

type doctorInput struct {
	Config string `json:"config,omitempty" jsonschema:"path to anyship.yaml, whose targets and options are checked; defaults to anyship.yaml in the server's working directory, and is optional"`
	Target string `json:"target,omitempty" jsonschema:"deploy target to check; defaults to the targets anyship.yaml names, or all of them"`
}

type doctorOutput struct {
	// OK is false when some check is an error.
	OK bool `json:"ok"`
	// Spec is the anyship.yaml whose options were used, when there was one.
	Spec    string         `json:"spec,omitempty"`
	Targets []doctorTarget `json:"targets"`
}

type doctorTarget struct {
	Name         string               `json:"name"`
	Dependencies []adapter.Dependency `json:"dependencies"`
	Findings     []adapter.Finding    `json:"findings"`
}

// newDoctorOutput checks this machine for target, or for the targets the
// spec at config names. A missing spec is fine unless it was asked for.
func (a *app) newDoctorOutput(ctx context.Context, config string, explicit bool, target string, aud audience) (doctorOutput, error) {
	out := doctorOutput{OK: true, Targets: []doctorTarget{}}
	var s *spec.Spec
	dir, err := os.Getwd()
	if err != nil {
		return out, err
	}
	if _, statErr := os.Stat(config); statErr == nil || explicit || !errors.Is(statErr, fs.ErrNotExist) {
		if s, err = load(config); err != nil {
			return out, aud.err(err)
		}
		if path, err := filepath.Abs(config); err == nil {
			dir, out.Spec = filepath.Dir(path), displayPath(path)
		}
	}

	names := a.registry.Names()
	switch {
	case target != "":
		if _, err := a.registry.Get(target); err != nil {
			return out, err
		}
		names = []string{target}
	case s != nil && len(s.Targets) > 0:
		names = slices.DeleteFunc(slices.Clone(names), func(name string) bool { _, ok := s.Targets[name]; return !ok })
	}

	env := &adapter.Env{Dir: dir, Logf: func(string, ...any) {}, Exec: run, LookupEnv: os.LookupEnv}
	for _, name := range names {
		ad, _ := a.registry.Get(name)
		t := doctorTarget{Name: name, Dependencies: []adapter.Dependency{}, Findings: []adapter.Finding{}}
		if doc, ok := ad.(adapter.Doctor); ok {
			c := doc.Doctor(ctx, s, env)
			t.Dependencies = append(t.Dependencies, c.Dependencies...)
			t.Findings = append(t.Findings, aud.findings(c.Findings)...)
		} else {
			t.Findings = append(t.Findings, adapter.Finding{Level: adapter.Info, Code: "DOCTOR_NOTHING_TO_CHECK",
				Message: fmt.Sprintf("The %s target has no checks for this machine.", name)})
		}
		if adapter.HasErrors(t.Findings) {
			out.OK = false
		}
		out.Targets = append(out.Targets, t)
	}
	return out, nil
}

func (a *app) printDoctor(out doctorOutput) {
	s := a.style
	if out.Spec != "" {
		fmt.Fprintln(a.out, s.dim("Using the options in "+out.Spec+"."))
	}
	var ready, failed []string
	for _, t := range out.Targets {
		fmt.Fprintln(a.out, "\n"+s.bold(t.Name))
		tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
		for _, tool := range t.Dependencies {
			// What a missing tool means is in the findings below it.
			icon, version := s.green("✔"), tool.Version
			if !tool.Found {
				icon, version = s.dim("-"), "not found"
			}
			fmt.Fprintf(tw, "  %s %s\t%s\t%s\t%s\n", icon, tool.Name, version, tool.Login, s.dim(tool.Need))
		}
		_ = tw.Flush()
		printFindings(a.out, s, t.Findings)
		if adapter.HasErrors(t.Findings) {
			failed = append(failed, t.Name)
		} else {
			ready = append(ready, t.Name)
		}
	}
	fmt.Fprintln(a.out)
	if len(ready) > 0 {
		fmt.Fprintf(a.out, "%s This machine can deploy to %s.\n", s.green("✔"), strings.Join(ready, ", "))
	}
	if len(failed) > 0 {
		fmt.Fprintf(a.out, "%s Fix the errors above to deploy to %s.\n", s.red("✖"), strings.Join(failed, ", "))
	}
}
