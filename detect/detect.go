// Package detect drafts an anyship.json from a project's files.
//
// Detection is rule-based and deterministic: the same repo always yields the
// same draft. It only drafts the spec — the user reviews and commits it, and
// every later deploy reads that file, not the detector.
package detect

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

// ServiceName is the name given to the single service a draft contains.
const ServiceName = "web"

type Detection struct {
	Spec     *spec.Spec
	Findings []adapter.Finding
	// Evidence lists the human-readable reasons behind each guess.
	Evidence []string
}

type frameworkRule struct {
	dep       string
	framework string
	kind      spec.ServiceKind
	output    string
	// edge marks frameworks known to run on edge runtimes such as Cloudflare Workers.
	edge bool
}

// First match wins, so meta-frameworks come before the libraries they build on.
var frameworks = []frameworkRule{
	{dep: "next", framework: "nextjs", kind: spec.KindServer},
	{dep: "nuxt", framework: "nuxt", kind: spec.KindServer},
	{dep: "@sveltejs/kit", framework: "sveltekit", kind: spec.KindServer},
	{dep: "astro", framework: "astro", kind: spec.KindStatic, output: "dist"},
	{dep: "hono", framework: "hono", kind: spec.KindServer, edge: true},
	{dep: "@nestjs/core", framework: "nestjs", kind: spec.KindServer},
	{dep: "fastify", framework: "fastify", kind: spec.KindServer},
	{dep: "express", framework: "express", kind: spec.KindServer},
	{dep: "react-scripts", framework: "create-react-app", kind: spec.KindStatic, output: "build"},
	{dep: "vite", framework: "vite", kind: spec.KindStatic, output: "dist"},
}

var lockfiles = []struct{ file, pm string }{
	{"bun.lock", "bun"},
	{"bun.lockb", "bun"},
	{"pnpm-lock.yaml", "pnpm"},
	{"yarn.lock", "yarn"},
	{"package-lock.json", "npm"},
}

var resourceDeps = []struct {
	dep string
	typ spec.ResourceType
}{
	{"pg", spec.ResourcePostgres},
	{"postgres", spec.ResourcePostgres},
	{"@neondatabase/serverless", spec.ResourcePostgres},
	{"mysql2", spec.ResourceMySQL},
	{"better-sqlite3", spec.ResourceSQLite},
	{"@libsql/client", spec.ResourceSQLite},
	{"ioredis", spec.ResourceRedis},
	{"redis", spec.ResourceRedis},
	{"@aws-sdk/client-s3", spec.ResourceBucket},
}

var resourceNames = map[spec.ResourceType]string{
	spec.ResourcePostgres: "db",
	spec.ResourceMySQL:    "db",
	spec.ResourceSQLite:   "db",
	spec.ResourceRedis:    "cache",
	spec.ResourceBucket:   "storage",
	spec.ResourceKV:       "kv",
}

var (
	// Node built-ins that edge runtimes don't provide at all.
	edgeBlockingModules = []string{"child_process", "cluster", "dgram", "worker_threads"}
	// Node built-ins edge runtimes only partly emulate.
	edgeLimitedModules = []string{"fs", "net"}
	// Dependencies that ship native binaries.
	nativeDeps = []string{"sharp", "bcrypt", "better-sqlite3", "sqlite3", "canvas", "argon2"}

	sourceExtensions = []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts"}
	skipDirs         = []string{"node_modules", "dist", "build", "coverage"}
	entryCandidates  = []string{"src/index.ts", "src/index.js", "src/worker.ts", "src/worker.js", "index.ts", "index.js"}
	otherLanguages   = []struct{ file, language string }{
		{"go.mod", "Go"}, {"requirements.txt", "Python"}, {"pyproject.toml", "Python"}, {"Cargo.toml", "Rust"},
	}

	importRe = regexp.MustCompile(`(?:from\s+|require\(\s*|import\(\s*)["'](?:node:)?(` +
		strings.Join(append(slices.Clone(edgeBlockingModules), edgeLimitedModules...), "|") +
		`)(?:/[^"']*)?["']`)
	portRe   = regexp.MustCompile(`(?:--port[= ]|-p[= ]|PORT=)(\d{2,5})\b`)
	exposeRe = regexp.MustCompile(`(?im)^\s*EXPOSE\s+(\d+)`)
)

const maxScannedFiles = 2000

type packageJSON struct {
	Name            string            `json:"name"`
	Main            string            `json:"main"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

// Project inspects dir and drafts a spec for it.
func Project(dir string) (*Detection, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	d := &Detection{}
	has := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }

	var pkg *packageJSON
	if has("package.json") {
		data, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			return nil, err
		}
		pkg = &packageJSON{}
		if err := json.Unmarshal(data, pkg); err != nil {
			return nil, fmt.Errorf("package.json: %w", err)
		}
	}

	name := filepath.Base(dir)
	if pkg != nil && pkg.Name != "" {
		name = pkg.Name
	}
	d.Spec = &spec.Spec{Version: spec.Version, Name: specName(name)}

	if pkg == nil {
		d.Spec.Services = map[string]*spec.Service{ServiceName: d.detectWithoutPackageJSON(dir, has)}
		return d, nil
	}
	d.detectJavaScript(dir, has, pkg)
	return d, nil
}

func (d *Detection) detectWithoutPackageJSON(dir string, has func(string) bool) *spec.Service {
	if has("Dockerfile") {
		d.Evidence = append(d.Evidence, "Dockerfile found → container service built from it")
		svc := &spec.Service{Kind: spec.KindServer, Dockerfile: "Dockerfile"}
		if port := dockerfilePort(filepath.Join(dir, "Dockerfile")); port > 0 {
			d.Evidence = append(d.Evidence, fmt.Sprintf("Dockerfile EXPOSE %d", port))
			svc.Ports = []spec.Port{{Port: port}}
		}
		return svc
	}
	if has("index.html") {
		d.Evidence = append(d.Evidence, "index.html without package.json → static site served from the project root")
		return &spec.Service{Kind: spec.KindStatic, Build: &spec.Build{Output: "."}}
	}
	for _, l := range otherLanguages {
		if has(l.file) {
			d.Evidence = append(d.Evidence, fmt.Sprintf("%s found → %s project", l.file, l.language))
		}
	}
	d.Findings = append(d.Findings, adapter.Finding{
		Level:   adapter.Error,
		Code:    "DETECT_UNKNOWN",
		Message: "Could not work out how to build or start this project.",
		Hint:    "Add a Dockerfile, or fill in services.web.start in anyship.json. Only JavaScript projects are auto-detected so far.",
	})
	return &spec.Service{Kind: spec.KindServer}
}

func (d *Detection) detectJavaScript(dir string, has func(string) bool, pkg *packageJSON) {
	deps := map[string]bool{}
	for dep := range pkg.Dependencies {
		deps[dep] = true
	}
	for dep := range pkg.DevDependencies {
		deps[dep] = true
	}

	pm := "npm"
	for _, l := range lockfiles {
		if has(l.file) {
			pm = l.pm
			break
		}
	}
	d.Evidence = append(d.Evidence, "package manager: "+pm)

	var rule *frameworkRule
	for i := range frameworks {
		if deps[frameworks[i].dep] {
			rule = &frameworks[i]
			d.Evidence = append(d.Evidence, fmt.Sprintf("dependency %q → %s (%s)", rule.dep, rule.framework, rule.kind))
			break
		}
	}

	svc := &spec.Service{Kind: spec.KindServer, Runtime: &spec.Runtime{Language: "javascript"}}
	if rule != nil {
		svc.Kind = rule.kind
		svc.Runtime.Framework = rule.framework
	}
	if pkg.Scripts["build"] != "" {
		svc.Build = &spec.Build{Command: pm + " run build"}
	}

	if svc.Kind == spec.KindStatic {
		if svc.Build == nil {
			svc.Build = &spec.Build{}
		}
		svc.Build.Output = cmp(rule.output, "dist")
	} else {
		d.detectServer(dir, has, pkg, pm, rule, svc)
	}

	d.Spec.Resources = map[string]*spec.Resource{}
	for _, rd := range resourceDeps {
		name := resourceNames[rd.typ]
		if !deps[rd.dep] || d.Spec.Resources[name] != nil {
			continue
		}
		d.Spec.Resources[name] = &spec.Resource{Type: rd.typ}
		svc.Uses = append(svc.Uses, name)
		d.Evidence = append(d.Evidence, fmt.Sprintf("dependency %q → %s resource %q", rd.dep, rd.typ, name))
	}

	if svc.Kind == spec.KindServer {
		blockers := d.scanEdgeCompatibility(dir, deps)
		switch {
		case blockers > 0:
			svc.Runtime.EdgeCompatible = ptr(false)
		case rule != nil && rule.edge:
			svc.Runtime.EdgeCompatible = ptr(true)
		}
	}

	d.Spec.Services = map[string]*spec.Service{ServiceName: svc}
}

func (d *Detection) detectServer(dir string, has func(string) bool, pkg *packageJSON, pm string, rule *frameworkRule, svc *spec.Service) {
	switch {
	case pkg.Scripts["start"] != "":
		svc.Start = pm + " run start"
	case pkg.Main != "":
		svc.Start = "node " + pkg.Main
	}

	if rule != nil && rule.edge {
		for _, candidate := range entryCandidates {
			if has(candidate) {
				svc.Entry = candidate
				d.Evidence = append(d.Evidence, "edge entry module: "+candidate)
				break
			}
		}
	}

	port := 0
	for _, script := range []string{"start", "dev", "serve", "preview"} {
		if m := portRe.FindStringSubmatch(pkg.Scripts[script]); m != nil {
			port, _ = strconv.Atoi(m[1])
			break
		}
	}
	switch {
	case port > 0:
		svc.Ports = []spec.Port{{Port: port}}
		d.Evidence = append(d.Evidence, fmt.Sprintf("port %d from package.json scripts", port))
	case svc.Start != "":
		svc.Ports = []spec.Port{{Port: 3000}}
		d.Findings = append(d.Findings, adapter.Finding{
			Level: adapter.Info, Code: "DETECT_PORT_ASSUMED", Message: "No port found in scripts; assumed 3000.", Service: ServiceName,
		})
	}

	if svc.Start == "" && svc.Entry == "" {
		d.Findings = append(d.Findings, adapter.Finding{
			Level:   adapter.Error,
			Code:    "DETECT_NO_START",
			Message: "No start script, main field or entry module found.",
			Service: ServiceName,
			Hint:    `Add a "start" script to package.json or set services.web.start in anyship.json.`,
		})
	}
}

// scanEdgeCompatibility records edge-runtime incompatibilities as findings and
// returns how many are blocking.
func (d *Detection) scanEdgeCompatibility(dir string, deps map[string]bool) int {
	blockers := 0
	for _, dep := range nativeDeps {
		if !deps[dep] {
			continue
		}
		blockers++
		d.Findings = append(d.Findings, adapter.Finding{
			Level:   adapter.Warning,
			Code:    "EDGE_NATIVE_DEPENDENCY",
			Message: fmt.Sprintf("%q ships native binaries, which edge runtimes cannot load.", dep),
			Service: ServiceName,
		})
	}

	for _, file := range sourceFiles(dir) {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var modules []string
		for _, m := range importRe.FindAllSubmatch(data, -1) {
			if mod := string(m[1]); !slices.Contains(modules, mod) {
				modules = append(modules, mod)
			}
		}
		rel, _ := filepath.Rel(dir, file)
		for _, mod := range modules {
			f := adapter.Finding{
				Level:   adapter.Info,
				Code:    "EDGE_LIMITED_MODULE",
				Message: fmt.Sprintf("imports node:%s, which edge runtimes only partly emulate.", mod),
				Service: ServiceName,
				File:    filepath.ToSlash(rel),
			}
			if slices.Contains(edgeBlockingModules, mod) {
				blockers++
				f.Level = adapter.Warning
				f.Code = "EDGE_UNSUPPORTED_MODULE"
				f.Message = fmt.Sprintf("imports node:%s, which edge runtimes do not provide.", mod)
			}
			d.Findings = append(d.Findings, f)
		}
	}
	return blockers
}

var errScanLimit = errors.New("scan limit reached")

// sourceFiles lists JavaScript/TypeScript sources under root in lexical order,
// skipping dependencies, build output and hidden directories.
func sourceFiles(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(entry.Name(), ".") || slices.Contains(skipDirs, entry.Name())) {
				return filepath.SkipDir
			}
			return nil
		}
		if slices.Contains(sourceExtensions, filepath.Ext(path)) && !strings.HasSuffix(path, ".d.ts") {
			out = append(out, path)
			if len(out) >= maxScannedFiles {
				return errScanLimit
			}
		}
		return nil
	})
	return out
}

func dockerfilePort(file string) int {
	data, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	if m := exposeRe.FindSubmatch(data); m != nil {
		port, _ := strconv.Atoi(string(m[1]))
		return port
	}
	return 0
}

var (
	scopeRe   = regexp.MustCompile(`^@[^/]+/`)
	invalidRe = regexp.MustCompile(`[^a-z0-9-]+`)
)

// specName turns a package or directory name into a valid spec name.
func specName(raw string) string {
	name := strings.ToLower(raw)
	name = scopeRe.ReplaceAllString(name, "")
	name = invalidRe.ReplaceAllString(name, "-")
	name = strings.TrimLeftFunc(name, func(r rune) bool { return r < 'a' || r > 'z' })
	name = strings.TrimRight(name, "-")
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	if name == "" {
		return "app"
	}
	return name
}

func cmp(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func ptr[T any](v T) *T { return &v }
