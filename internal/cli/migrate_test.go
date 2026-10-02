package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/j75689/anyship/spec"
)

const legacySpec = `{
  "version": 1,
  "name": "shop",
  "services": {"web": {"kind": "server", "start": "node server.js", "ports": [{"port": 3000}]}}
}`

// migrate runs `anyship migrate dir` with the given flags and returns what the
// command printed.
func migrate(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	a := &app{out: &out, style: styler{}}
	cmd := a.migrateCommand()
	cmd.SetArgs(append([]string{dir}, args...))
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	return out.String(), err
}

func writeLegacy(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, spec.LegacyFilename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMigrateWritesTheManifest(t *testing.T) {
	dir := writeLegacy(t, legacySpec)
	out, err := migrate(t, dir)
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "wrote ") {
		t.Errorf("output:\n%s", out)
	}

	written, err := os.ReadFile(filepath.Join(dir, spec.Filename))
	if err != nil {
		t.Fatal(err)
	}
	s, err := spec.Parse(written)
	if err != nil {
		t.Fatalf("the written spec does not parse: %v\n%s", err, written)
	}
	if s.Name != "shop" {
		t.Errorf("metadata.name = %q, want shop", s.Name)
	}
	// Migrating converts; it does not clean up for you.
	if _, err := os.Stat(filepath.Join(dir, spec.LegacyFilename)); err != nil {
		t.Errorf("%s was removed: %v", spec.LegacyFilename, err)
	}
}

func TestMigrateDryRunPrintsWithoutWriting(t *testing.T) {
	dir := writeLegacy(t, legacySpec)
	out, err := migrate(t, dir, "--dry-run")
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "apiVersion: "+spec.APIVersion) || !strings.Contains(out, "name: shop") {
		t.Errorf("output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, spec.Filename)); err == nil {
		t.Errorf("--dry-run wrote %s", spec.Filename)
	}
}

func TestMigrateRefusesToOverwriteWithoutForce(t *testing.T) {
	dir := writeLegacy(t, legacySpec)
	existing := filepath.Join(dir, spec.Filename)
	if err := os.WriteFile(existing, []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := migrate(t, dir)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("error = %v, want it to suggest --force", err)
	}
	if kept, _ := os.ReadFile(existing); string(kept) != "# mine\n" {
		t.Errorf("%s was overwritten: %s", spec.Filename, kept)
	}

	// --dry-run never writes, so an existing file is not in its way.
	if out, err := migrate(t, dir, "--dry-run"); err != nil {
		t.Errorf("migrate --dry-run: %v\n%s", err, out)
	}

	if _, err := migrate(t, dir, "--force"); err != nil {
		t.Fatalf("migrate --force: %v", err)
	}
	if replaced, _ := os.ReadFile(existing); !strings.Contains(string(replaced), "name: shop") {
		t.Errorf("--force did not replace %s: %s", spec.Filename, replaced)
	}
}

func TestMigrateWithoutALegacyFile(t *testing.T) {
	_, err := migrate(t, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "nothing to migrate") {
		t.Fatalf("error = %v, want it to say there is nothing to migrate", err)
	}
}

func TestMigrateReportsFieldsItCannotConvert(t *testing.T) {
	dir := writeLegacy(t, `{"version": 1, "name": "shop", "services": {}, "region": "eu"}`)
	_, err := migrate(t, dir)
	if err == nil || !strings.Contains(err.Error(), "region: unknown field") {
		t.Fatalf("error = %v, want it to name the unknown field", err)
	}
	if _, err := os.Stat(filepath.Join(dir, spec.Filename)); err == nil {
		t.Errorf("a refused migration still wrote %s", spec.Filename)
	}
}

// A spec that was already invalid is still converted: fixing it in the new
// format beats fixing it in the old one. The command says so and exits non-zero.
func TestMigrateReportsValidationProblemsButKeepsTheFile(t *testing.T) {
	dir := writeLegacy(t, `{"version": 1, "name": "shop",
		"services": {"web": {"kind": "server", "start": "x", "uses": ["db"]}}}`)
	out, err := migrate(t, dir)
	if err == nil {
		t.Fatalf("migrate accepted an invalid spec:\n%s", out)
	}
	for _, want := range []string{"wrote ", "needs edits", `unknown resource "db"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, spec.Filename)); err != nil {
		t.Errorf("%s was not written: %v", spec.Filename, err)
	}
}
