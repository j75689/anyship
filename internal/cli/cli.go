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
	"github.com/j75689/anyship/adapters/vps"
	"github.com/j75689/anyship/detect"
	"github.com/j75689/anyship/diagnose"
	"github.com/j75689/anyship/schema"
	"github.com/j75689/anyship/spec"
)

// errReported means the command already explained the failure to the user.
var errReported = errors.New("failure already reported")

type app struct {
	registry *adapter.Registry
	out      io.Writer
	style    styler
	// newModel builds the diagnosis model; tests replace it.
	newModel func(model, effort string) diagnose.Model
}

// Execute runs the CLI and returns the process exit code.
func Execute(version string) int {
	style := newStyler(os.Stderr)
	registry, err := adapter.NewRegistry(aws.New(), cloudflare.New(), gcp.New(), vps.New())
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
	root.AddCommand(a.initCommand(), a.validateCommand(), a.planCommand(), a.applyCommand(), a.logsCommand(),
		a.statusCommand(), a.destroyCommand(), a.diagnoseCommand(), a.targetsCommand(), a.schemaCommand(), a.mcpCommand(version))
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
	var force bool
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Detect the project and draft " + spec.Filename,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			root, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			file := filepath.Join(root, spec.Filename)
			if _, err := os.Stat(file); err == nil && !force {
				return fmt.Errorf("%s already exists; pass --force to overwrite it", spec.Filename)
			}

			d, err := detect.Project(root)
			if err != nil {
				return err
			}
			fmt.Fprintln(a.out, a.style.bold("Detected:"))
			for _, line := range d.Evidence {
				fmt.Fprintf(a.out, "  %s %s\n", a.style.dim("•"), line)
			}
			if len(d.Findings) > 0 {
				fmt.Fprintln(a.out, a.style.bold("\nFindings:"))
				printFindings(a.out, a.style, d.Findings)
			}

			data, err := spec.Marshal(d.Spec)
			if err != nil {
				return err
			}
			if err := os.WriteFile(file, data, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "\n%s wrote %s\n", a.style.green("✔"), displayPath(file))

			// The spec's editor hint points at this copy, so write it too.
			schemaFile := filepath.Join(root, filepath.FromSlash(spec.SchemaFile))
			if err := writeSchema(schemaFile); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "%s wrote %s\n", a.style.green("✔"), displayPath(schemaFile))

			if _, err := spec.Parse(data); err != nil {
				fmt.Fprintln(a.out, a.style.yellow("\nThe draft needs edits before it can be deployed:"))
				for _, problem := range problemsOf(err) {
					fmt.Fprintf(a.out, "  %s %s\n", a.style.red("✖"), problem)
				}
				return errReported
			}
			fmt.Fprintln(a.out, a.style.dim("Review it, commit it, then run `anyship plan --target <target>`."))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite an existing "+spec.Filename)
	return cmd
}

func (a *app) validateCommand() *cobra.Command {
	var config string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check " + spec.Filename + " against the schema",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
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
	return cmd
}

func (a *app) planCommand() *cobra.Command {
	var config, target string
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
			p, err := d.adapter.Plan(cmd.Context(), d.spec, d.env)
			if err != nil {
				return err
			}
			if asJSON {
				err = printJSON(a.out, planJSON(p))
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
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func (a *app) applyCommand() *cobra.Command {
	var config, target string
	var yes, dryRun bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Deploy to a target",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.prepare(config, target, dryRun)
			if err != nil {
				return err
			}
			p, err := d.adapter.Plan(cmd.Context(), d.spec, d.env)
			if err != nil {
				return err
			}
			printPlan(a.out, a.style, p, d.env.Dir)
			if adapter.HasErrors(p.Findings) {
				return errReported
			}
			if !yes && !dryRun {
				ok, err := confirm(a.out, fmt.Sprintf("\nDeploy %s to %s?", d.spec.Name, d.adapter.Name()))
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(a.out, "Aborted.")
					return nil
				}
			}

			result, err := d.adapter.Apply(cmd.Context(), p, d.spec, d.env)
			if err != nil {
				return err
			}
			printResult(a.out, a.style, result)
			if !result.OK {
				return errReported
			}
			return nil
		},
	}
	addConfigFlag(cmd, &config)
	addTargetFlag(cmd, &target)
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "generate config and run the platform's dry run without deploying")
	return cmd
}

func (a *app) targetsCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "targets",
		Short: "List available deploy targets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			type targetJSON struct {
				Name         string   `json:"name"`
				Description  string   `json:"description"`
				Capabilities []string `json:"capabilities"`
			}
			var targets []targetJSON
			for _, ad := range a.registry.List() {
				targets = append(targets, targetJSON{ad.Name(), ad.Description(), capabilities(ad)})
			}
			if asJSON {
				return printJSON(a.out, targets)
			}
			for _, t := range targets {
				fmt.Fprintf(a.out, "  %s %s %s\n", a.style.bold(fmt.Sprintf("%-12s", t.Name)), t.Description, a.style.dim("("+strings.Join(t.Capabilities, ", ")+")"))
			}
			return nil
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
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

func addJSONFlag(cmd *cobra.Command, asJSON *bool) {
	cmd.Flags().BoolVar(asJSON, "json", false, "print machine-readable JSON")
}

func (a *app) schemaCommand() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema for " + spec.Filename,
		Long: "Print the JSON Schema for " + spec.Filename + ".\n\n" +
			"The schema ships inside this binary, so editors can validate the spec\n" +
			"without downloading anything.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "" {
				_, err := a.out.Write(schema.JSON)
				return err
			}
			if err := writeSchema(out); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "%s wrote %s\n", a.style.green("✔"), displayPath(out))
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "write the schema to this file instead of stdout")
	return cmd
}

// writeSchema writes the embedded schema to path, creating its directory.
func writeSchema(path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, schema.JSON, 0o644)
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
	return nil, fmt.Errorf("%s: %w", config, err)
}

func problemsOf(err error) []string {
	var verr *spec.ValidationError
	if errors.As(err, &verr) {
		return verr.Problems
	}
	return nil
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
	case runtime.GOOS == "windows":
		// npx and friends are .cmd shims on Windows, which only run through cmd.
		cmd = exec.CommandContext(ctx, "cmd", append([]string{"/C", name}, args...)...)
	default:
		cmd = exec.CommandContext(ctx, name, args...)
	}
	cmd.Dir = opts.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = std.in, std.out, std.err
	if opts.Stdin != nil {
		cmd.Stdin = opts.Stdin
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
