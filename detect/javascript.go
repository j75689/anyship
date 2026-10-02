package detect

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

type frameworkRule struct {
	dep       string
	framework string
	kind      spec.ServiceKind
	output    string
	// edge marks frameworks known to run on edge runtimes such as Cloudflare Workers.
	edge bool
}

// First match wins, so meta-frameworks come before the libraries they build on.
var jsFrameworks = []frameworkRule{
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

var jsResourceDeps = []struct {
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

var (
	// Node built-ins that edge runtimes don't provide at all.
	edgeBlockingModules = []string{"child_process", "cluster", "dgram", "worker_threads"}
	// Node built-ins edge runtimes only partly emulate.
	edgeLimitedModules = []string{"fs", "net"}
	// Dependencies that ship native binaries.
	nativeDeps = []string{"sharp", "bcrypt", "better-sqlite3", "sqlite3", "canvas", "argon2"}

	jsExtensions    = []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts"}
	entryCandidates = []string{"src/index.ts", "src/index.js", "src/worker.ts", "src/worker.js", "index.ts", "index.js"}

	importRe = regexp.MustCompile(`(?:from\s+|require\(\s*|import\(\s*)["'](?:node:)?(` +
		strings.Join(append(slices.Clone(edgeBlockingModules), edgeLimitedModules...), "|") +
		`)(?:/[^"']*)?["']`)
	scriptPortRe = regexp.MustCompile(`(?:--port[= ]|-p[= ]|PORT=)(\d{2,5})\b`)
)

type packageJSON struct {
	Name            string            `json:"name"`
	Main            string            `json:"main"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

func (d *Detection) detectJavaScript(p project) (*spec.Service, error) {
	var pkg packageJSON
	if err := json.Unmarshal([]byte(p.read("package.json")), &pkg); err != nil {
		return nil, fmt.Errorf("package.json: %w", err)
	}
	d.setName(pkg.Name)

	deps := map[string]bool{}
	for dep := range pkg.Dependencies {
		deps[dep] = true
	}
	for dep := range pkg.DevDependencies {
		deps[dep] = true
	}

	pm := "npm"
	for _, l := range lockfiles {
		if p.has(l.file) {
			pm = l.pm
			break
		}
	}
	d.Evidence = append(d.Evidence, "package manager: "+pm)

	var rule *frameworkRule
	for i := range jsFrameworks {
		if deps[jsFrameworks[i].dep] {
			rule = &jsFrameworks[i]
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
		svc.Build.Output = rule.output
	} else {
		d.detectJavaScriptServer(p, &pkg, pm, rule, svc)
	}

	for _, rd := range jsResourceDeps {
		if deps[rd.dep] {
			d.addResource(svc, rd.typ, fmt.Sprintf("dependency %q", rd.dep))
		}
	}

	if svc.Kind == spec.KindServer {
		blockers := d.scanEdgeCompatibility(p, deps)
		switch {
		case blockers > 0:
			svc.Runtime.EdgeCompatible = ptr(false)
		case rule != nil && rule.edge:
			svc.Runtime.EdgeCompatible = ptr(true)
		}
	}
	return svc, nil
}

func (d *Detection) detectJavaScriptServer(p project, pkg *packageJSON, pm string, rule *frameworkRule, svc *spec.Service) {
	switch {
	case pkg.Scripts["start"] != "":
		svc.Start = pm + " run start"
	case pkg.Main != "":
		svc.Start = "node " + pkg.Main
	}

	if rule != nil && rule.edge {
		for _, candidate := range entryCandidates {
			if p.has(candidate) {
				svc.Entry = candidate
				d.Evidence = append(d.Evidence, "edge entry module: "+candidate)
				break
			}
		}
	}

	port := 0
	for _, script := range []string{"start", "dev", "serve", "preview"} {
		if m := scriptPortRe.FindStringSubmatch(pkg.Scripts[script]); m != nil {
			port, _ = strconv.Atoi(m[1])
			break
		}
	}
	switch {
	case port > 0:
		svc.Ports = []spec.Port{{Port: port}}
		d.Evidence = append(d.Evidence, fmt.Sprintf("port %d from package.json scripts", port))
	case svc.Start != "":
		d.usePort(svc, listenAddress{}, false, 3000)
	}

	if svc.Start == "" && svc.Entry == "" {
		d.Findings = append(d.Findings, adapter.Finding{
			Level:   adapter.Error,
			Code:    "DETECT_NO_START",
			Message: "No start script, main field or entry module found.",
			Service: ServiceName,
			Hint:    `Add a "start" script to package.json or set services.web.start in anyship.yaml.`,
		})
	}
}

// scanEdgeCompatibility records edge-runtime incompatibilities as findings and
// returns how many are blocking.
func (d *Detection) scanEdgeCompatibility(p project, deps map[string]bool) int {
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

	for _, file := range p.sourceFiles(jsExtensions...) {
		var modules []string
		for _, m := range importRe.FindAllStringSubmatch(p.read(file), -1) {
			if !slices.Contains(modules, m[1]) {
				modules = append(modules, m[1])
			}
		}
		for _, mod := range modules {
			f := adapter.Finding{
				Level:   adapter.Info,
				Code:    "EDGE_LIMITED_MODULE",
				Message: fmt.Sprintf("imports node:%s, which edge runtimes only partly emulate.", mod),
				Service: ServiceName,
				File:    file,
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
