package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/j75689/anyship/spec"
)

func (a *app) migrateCommand() *cobra.Command {
	var dryRun, force bool
	cmd := &cobra.Command{
		Use:   "migrate [dir]",
		Short: "Convert a v0.2 " + spec.LegacyFilename + " into " + spec.Filename,
		Long: fmt.Sprintf(`Convert a v0.2 %s into %s.

Only the envelope changed: version and name become apiVersion, kind and
metadata.name, and the rest moves under spec untouched. Fields anyship no
longer recognizes are reported rather than dropped.`, spec.LegacyFilename, spec.Filename),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			root, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			from, to := filepath.Join(root, spec.LegacyFilename), filepath.Join(root, spec.Filename)

			legacy, err := os.ReadFile(from)
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("no %s in %s; nothing to migrate", spec.LegacyFilename, displayPath(root))
			}
			if err != nil {
				return err
			}
			if _, err := os.Stat(to); err == nil && !force && !dryRun {
				return fmt.Errorf("%s already exists; pass --force to overwrite it", spec.Filename)
			}

			data, err := spec.Migrate(legacy)
			if err != nil {
				if problems := problemsOf(err); len(problems) > 0 {
					return fmt.Errorf("%s cannot be migrated:\n  ✖ %s", displayPath(from), strings.Join(problems, "\n  ✖ "))
				}
				return fmt.Errorf("%s: %w", displayPath(from), err)
			}

			if dryRun {
				if _, err := a.out.Write(data); err != nil {
					return err
				}
			} else {
				if err := os.WriteFile(to, data, 0o644); err != nil {
					return err
				}
				fmt.Fprintf(a.out, "%s wrote %s\n", a.style.green("✔"), displayPath(to))
			}

			// The envelope is right by construction, so anything left is a
			// problem the old spec already had. Say so instead of hiding it.
			if _, err := spec.Parse(data); err != nil {
				problems := problemsOf(err)
				if len(problems) == 0 {
					return err
				}
				fmt.Fprintln(a.out, a.style.yellow("\nThe converted spec needs edits before it can be deployed:"))
				for _, problem := range problems {
					fmt.Fprintf(a.out, "  %s %s\n", a.style.red("✖"), problem)
				}
				return errReported
			}
			if !dryRun {
				fmt.Fprintln(a.out, a.style.dim("Review it, commit it, then delete "+spec.LegacyFilename+"."))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the converted manifest instead of writing it")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite an existing "+spec.Filename)
	return cmd
}
