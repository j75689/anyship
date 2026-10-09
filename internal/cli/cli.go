// Package cli implements the anyship command.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/adapters/aws"
	"github.com/j75689/anyship/adapters/cloudflare"
	"github.com/j75689/anyship/adapters/gcp"
	"github.com/j75689/anyship/adapters/kubernetes"
	"github.com/j75689/anyship/adapters/vps"
	"github.com/j75689/anyship/spec"
)

// errReported means the command already explained the failure to the user.
var errReported = errors.New("failure already reported")

type app struct {
	registry *adapter.Registry
	out      io.Writer
	style    styler
}

// Execute runs the CLI and returns the process exit code.
func Execute(version string) int {
	style := newStyler(os.Stderr)
	registry, err := adapter.NewRegistry(aws.New(), cloudflare.New(), gcp.New(), kubernetes.New(), vps.New())
	if err != nil {
		fmt.Fprintln(os.Stderr, style.red("error: "+err.Error()))
		return 1
	}
	a := &app{registry: registry, out: os.Stdout, style: newStyler(os.Stdout)}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := a.rootCommand(version).ExecuteContext(ctx); err != nil {
		if !errors.Is(err, errReported) {
			fmt.Fprintln(os.Stderr, style.red("error: "+err.Error()))
		}
		return 1
	}
	return 0
}

func (a *app) rootCommand(version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "anyship",
		Short:         "Deploy any app to any platform from one spec.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(a.initCommand(), a.validateCommand(), a.planCommand(), a.applyCommand(),
		a.logsCommand(), a.statusCommand(), a.destroyCommand(), a.diagnoseCommand(), a.doctorCommand(), a.targetsCommand(),
		a.schemaCommand(), a.mcpCommand(version))
	return root
}

func (a *app) logsCommand() *cobra.Command {
	var config, target, since string
	var follow, timestamps bool
	var tail int
	cmd := &cobra.Command{
		Use:   "logs [service]",
		Short: "Show runtime logs from a deployed target",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.prepare(config, target, false)
			if err != nil {
				return err
			}
			reader, ok := d.adapter.(adapter.LogReader)
			if !ok {
				return fmt.Errorf("the %s target does not provide logs", d.adapter.Name())
			}

			opts := adapter.LogOptions{Follow: follow, Since: since, Timestamps: timestamps}
			if cmd.Flags().Changed("tail") {
				if tail < 1 {
					return errors.New("--tail must be at least 1")
				}
				opts.Tail = tail
			}
			if since != "" && !validSince(since) {
				return fmt.Errorf(`--since %q is not a duration like "10m" or a timestamp like "2026-10-01T12:00:00Z"`, since)
			}
			if len(args) == 1 {
				if _, ok := d.spec.Services[args[0]]; !ok {
					return fmt.Errorf("unknown service %q; services: %s", args[0], strings.Join(d.spec.ServiceNames(), ", "))
				}
				opts.Service = args[0]
			}

			err = reader.Logs(cmd.Context(), d.spec, d.env, opts)
			if err != nil && cmd.Context().Err() != nil {
				return nil // interrupted while following: a normal way to stop
			}
			return err
		},
	}
	addConfigFlag(cmd, &config)
	addTargetFlag(cmd, &target)
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming new log lines")
	cmd.Flags().IntVarP(&tail, "tail", "n", 0, "number of recent lines per service (default depends on the target)")
	cmd.Flags().StringVar(&since, "since", "", `only show logs newer than a duration ("10m") or timestamp`)
	cmd.Flags().BoolVar(&timestamps, "timestamps", false, "prefix each line with its timestamp")
	return cmd
}

// validSince accepts what both docker and anyship can interpret unambiguously.
func validSince(s string) bool {
	if d, err := time.ParseDuration(s); err == nil {
		return d > 0
	}
	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if _, err := time.Parse(layout, s); err == nil {
			return true
		}
	}
	return false
}

func (a *app) initCommand() *cobra.Command {
	var force, toStdout, asJSON bool
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Detect the project and draft " + spec.Filename,
		Long: "Detect the project and draft " + spec.Filename + ".\n\n" +
			"--stdout prints the draft instead of writing it, and --json prints it with the\n" +
			"evidence, findings and whether the directory already has a spec; neither writes.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			out, err := newDetectOutput(dir, shell)
			if err != nil {
				return err
			}
			if asJSON {
				if err := printJSON(a.out, out); err != nil {
					return err
				}
				if !out.Valid {
					return errReported
				}
				return nil
			}
			if toStdout {
				// The draft alone on stdout, so it can be piped; the rest on stderr.
				a.printDetection(os.Stderr, newStyler(os.Stderr), out)
				if _, err := io.WriteString(a.out, out.Spec); err != nil {
					return err
				}
				if !out.Valid {
					return errReported
				}
				return nil
			}

			file := filepath.Join(out.Dir, spec.Filename)
			if out.Existing != nil && !force {
				return fmt.Errorf("%s already exists; pass --force to overwrite it, or --stdout to print the draft instead", spec.Filename)
			}
			a.printDetection(a.out, a.style, out)
			if err := os.WriteFile(file, []byte(out.Spec), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "\n%s wrote %s\n", a.style.green("✔"), displayPath(file))
			if !out.Valid {
				return errReported
			}
			fmt.Fprintln(a.out, a.style.dim("Review it, commit it, then run `anyship plan --target <target>`."))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite an existing "+spec.Filename)
	cmd.Flags().BoolVar(&toStdout, "stdout", false, "print the draft instead of writing "+spec.Filename)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// printDetection shows what init found, and what the draft still needs.
func (a *app) printDetection(w io.Writer, s styler, out detectOutput) {
	fmt.Fprintln(w, s.bold("Detected:"))
	for _, line := range out.Evidence {
		fmt.Fprintf(w, "  %s %s\n", s.dim("•"), line)
	}
	if len(out.Findings) > 0 {
		fmt.Fprintln(w, s.bold("\nFindings:"))
		printFindings(w, s, out.Findings)
	}
	if !out.Valid {
		fmt.Fprintln(w, s.yellow("\nThe draft needs edits before it can be deployed:"))
		for _, problem := range out.Problems {
			fmt.Fprintf(w, "  %s %s\n", s.red("✖"), problem)
		}
	}
}

func (a *app) validateCommand() *cobra.Command {
	var config string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check " + spec.Filename + " against the schema",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if asJSON {
				out := newValidateOutput(config, shell)
				if err := printJSON(a.out, out); err != nil {
					return err
				}
				if !out.Valid {
					return errReported
				}
				return nil
			}
			s, err := load(config)
			if err != nil {
				return err
			}
			names := s.ServiceNames()
			fmt.Fprintf(a.out, "%s %s: %d service(s) (%s), %d resource(s)\n",
				a.style.green("✔"), s.Name, len(names), strings.Join(names, ", "), len(s.Resources))
			return nil
		},
	}
	addConfigFlag(cmd, &config)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func (a *app) planCommand() *cobra.Command {
	var config, target string
	var images []string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show what a deploy to a target would do, without changing anything",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.prepare(config, target, false)
			if err != nil {
				return err
			}
			if err := useImages(d.spec, images); err != nil {
				return err
			}
			p, err := d.adapter.Plan(cmd.Context(), d.spec, d.env)
			if err != nil {
				return err
			}
			if asJSON {
				err = printJSON(a.out, newPlanOutput(p, shell))
			} else {
				printPlan(a.out, a.style, p, d.env.Dir)
			}
			if err == nil && adapter.HasErrors(p.Findings) {
				return errReported
			}
			return err
		},
	}
	addConfigFlag(cmd, &config)
	addTargetFlag(cmd, &target)
	addImageFlag(cmd, &images)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func (a *app) applyCommand() *cobra.Command {
	var config, target string
	var images []string
	var yes, dryRun, asJSON bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Deploy to a target",
		Long: "Deploy to a target.\n\n" +
			"With --json the outcome is printed as JSON, with the tail of what the platform's\n" +
			"tools printed; progress and that output go to stderr as they happen.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.prepare(config, target, dryRun)
			if err != nil {
				return err
			}
			if err := useImages(d.spec, images); err != nil {
				return err
			}
			var output *tailBuffer
			if asJSON {
				output = a.keepStdoutForJSON(d)
			}
			p, err := d.adapter.Plan(cmd.Context(), d.spec, d.env)
			if err != nil {
				return err
			}
			if asJSON {
				if adapter.HasErrors(p.Findings) {
					if err := printJSON(a.out, planErrorsOutput(p, shell)); err != nil {
						return err
					}
					return errReported
				}
			} else {
				printPlan(a.out, a.style, p, d.env.Dir)
				if adapter.HasErrors(p.Findings) {
					return errReported
				}
			}
			if !yes && !dryRun {
				ok, err := confirm(a.promptWriter(asJSON), fmt.Sprintf("\nDeploy %s to %s?", d.spec.Name, d.adapter.Name()))
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(a.promptWriter(asJSON), "Aborted.")
					return nil
				}
			}

			result, err := d.adapter.Apply(cmd.Context(), p, d.spec, d.env)
			if err != nil {
				return err
			}
			if asJSON {
				out := newResultOutput(result, nil, output, shell)
				for _, f := range p.Files {
					out.Files = append(out.Files, f.Path)
				}
				if err := printJSON(a.out, out); err != nil {
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
	addImageFlag(cmd, &images)
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "generate config and run the platform's dry run without deploying")
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// keepStdoutForJSON routes a deployment's progress and the output of the
// platform's tools to stderr, and keeps their tail for the JSON result, so
// stdout carries the JSON alone.
func (a *app) keepStdoutForJSON(d *deployment) *tailBuffer {
	output := &tailBuffer{max: maxToolOutput}
	d.env.Logf = func(format string, args ...any) {
		a.logToStderr(format, args...)
		fmt.Fprintf(output, format+"\n", args...)
	}
	d.env.Exec = func(ctx context.Context, opts adapter.ExecOptions, name string, args ...string) error {
		both := io.MultiWriter(os.Stderr, output)
		return runWith(ctx, opts, stdio{in: os.Stdin, out: both, err: both}, name, args...)
	}
	return output
}

// promptWriter is where a question to the user goes: stdout, unless stdout
// is reserved for JSON.
func (a *app) promptWriter(asJSON bool) io.Writer {
	if asJSON {
		return os.Stderr
	}
	return a.out
}

func (a *app) targetsCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "targets [name]",
		Short: "List available deploy targets, or print one target's page",
		Long: "List available deploy targets.\n\n" +
			"With a name, print that target's page: its options under spec.targets.<name>,\n" +
			"how it deploys a spec, what it refuses and why.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return a.printTargetPage(args[0], asJSON)
			}
			out := newTargetsOutput(a.registry, targetsCommandFor)
			if asJSON {
				return printJSON(a.out, out.Targets)
			}
			for _, t := range out.Targets {
				fmt.Fprintf(a.out, "  %s %s %s\n", a.style.bold(fmt.Sprintf("%-12s", t.Name)), t.Description, a.style.dim("("+strings.Join(t.Capabilities, ", ")+")"))
			}
			fmt.Fprintln(a.out, a.style.dim("\n`anyship targets <name>` prints a target's page: its options, how it deploys, what it refuses."))
			return nil
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// targetsCommandFor is how a shell reads a target's page.
func targetsCommandFor(name string) string { return "anyship targets " + name }

func (a *app) printTargetPage(name string, asJSON bool) error {
	ad, err := a.registry.Get(name)
	if err != nil {
		return err
	}
	page, err := targetDoc(name)
	if err != nil {
		return fmt.Errorf("the %s target has no page yet", name)
	}
	if asJSON {
		return printJSON(a.out, targetPage{targetInfo: newTargetInfo(ad, targetsCommandFor), Page: string(page)})
	}
	_, err = a.out.Write(page)
	return err
}

func addJSONFlag(cmd *cobra.Command, asJSON *bool) {
	cmd.Flags().BoolVar(asJSON, "json", false, "print machine-readable JSON")
}

func (a *app) schemaCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema for " + spec.Filename,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := spec.JSONSchema()
			if err != nil {
				return err
			}
			_, err = a.out.Write(data)
			return err
		},
	}
}

func addConfigFlag(cmd *cobra.Command, config *string) {
	cmd.Flags().StringVarP(config, "config", "c", spec.Filename, "spec file")
}

func addTargetFlag(cmd *cobra.Command, target *string) {
	// No backticks: pflag would show the quoted text as the flag's value name.
	cmd.Flags().StringVarP(target, "target", "t", "", `deploy target; "anyship targets" lists them`)
	_ = cmd.MarkFlagRequired("target")
}

type deployment struct {
	spec    *spec.Spec
	adapter adapter.Adapter
	env     *adapter.Env
}

func (a *app) prepare(config, target string, dryRun bool) (*deployment, error) {
	ad, err := a.registry.Get(target)
	if err != nil {
		return nil, err
	}
	s, err := load(config)
	if err != nil {
		return nil, err
	}
	path, err := filepath.Abs(config)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	return &deployment{
		spec:    s,
		adapter: ad,
		env: &adapter.Env{
			Dir:       dir,
			OutDir:    filepath.Join(dir, ".anyship", ad.Name()),
			DryRun:    dryRun,
			Logf:      func(format string, args ...any) { fmt.Fprintln(a.out, a.style.dim(fmt.Sprintf(format, args...))) },
			Exec:      run,
			LookupEnv: os.LookupEnv,
		},
	}, nil
}

func load(config string) (*spec.Spec, error) {
	s, err := spec.Load(config)
	if err == nil {
		return s, nil
	}
	if problems := problemsOf(err); len(problems) > 0 {
		return nil, fmt.Errorf("%s is invalid:\n  ✖ %s", config, strings.Join(problems, "\n  ✖ "))
	}
	var missing *spec.NotFoundError
	if errors.As(err, &missing) {
		return nil, hintForShell(err)
	}
	return nil, fmt.Errorf("%s: %w", config, err)
}

// run executes a command with inherited stdio, or opts.Stdin/opts.Stdout when set.
func run(ctx context.Context, opts adapter.ExecOptions, name string, args ...string) error {
	return runWith(ctx, opts, stdio{in: os.Stdin, out: os.Stdout, err: os.Stderr}, name, args...)
}

// stdio is where a command's streams go unless ExecOptions overrides them.
type stdio struct {
	in       io.Reader
	out, err io.Writer
}

func runWith(ctx context.Context, opts adapter.ExecOptions, std stdio, name string, args ...string) error {
	var cmd *exec.Cmd
	switch {
	case opts.Shell && runtime.GOOS == "windows":
		cmd = exec.CommandContext(ctx, "cmd", "/C", strings.Join(append([]string{name}, args...), " "))
	case opts.Shell:
		cmd = exec.CommandContext(ctx, "sh", "-c", strings.Join(append([]string{name}, args...), " "))
	default:
		// Windows used to go through `cmd /C` here, for .cmd shims like npx.
		// os/exec already finds those through PATHEXT and quotes arguments the
		// way cmd.exe reads them, while the wrapper made cmd.exe re-parse every
		// argument and mangled the ones holding a quote, & or ^ — such as the
		// remote script the vps target hands to ssh.
		cmd = exec.CommandContext(ctx, name, args...)
	}
	cmd.Dir = opts.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = std.in, std.out, std.err
	if opts.Stdin != nil {
		cmd.Stdin = opts.Stdin
	}
	if opts.Stderr != nil {
		cmd.Stderr = opts.Stderr
	}
	if opts.Stdout != nil {
		cmd.Stdout = opts.Stdout
	}
	if len(opts.Env) > 0 {
		cmd.Env = append(os.Environ(), opts.Env...)
	}
	return cmd.Run()
}

func confirm(w io.Writer, question string) (bool, error) {
	if !isTerminal(os.Stdin) {
		return false, errors.New("refusing to continue without confirmation in a non-interactive shell; pass --yes")
	}
	fmt.Fprintf(w, "%s [y/N] ", question)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

// displayPath shows a path relative to the working directory when it is inside it.
func displayPath(path string) string {
	wd, err := os.Getwd()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(wd, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}
