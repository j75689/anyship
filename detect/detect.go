// Package detect drafts an anyship.yaml from a project's files.
//
// Detection is rule-based and deterministic: the same repo always yields the
// same draft. It only drafts the spec — the user reviews and commits it, and
// every later deploy reads that file, not the detector.
package detect

import (
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

// Project inspects dir and drafts a spec for it.
func Project(dir string) (*Detection, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	p := project{dir: dir}
	d := &Detection{Spec: &spec.Spec{Name: specName(filepath.Base(dir)), Resources: map[string]*spec.Resource{}}}
	var svc *spec.Service
	switch {
	case p.has("package.json"):
		svc, err = d.detectJavaScript(p)
	case p.has("go.mod"):
		svc = d.detectGo(p)
	case p.has("Cargo.toml"):
		svc = d.detectRust(p)
	case p.has("pyproject.toml") || p.has("requirements.txt"):
		svc = d.detectPython(p)
	case p.has("Dockerfile"):
		svc = d.detectDockerfile(p)
	case p.has("index.html"):
		d.Evidence = append(d.Evidence, "index.html without a manifest → static site served from the project root")
		svc = &spec.Service{Kind: spec.KindStatic, Build: &spec.Build{Output: "."}}
	default:
		d.Findings = append(d.Findings, adapter.Finding{
			Level:   adapter.Error,
			Code:    "DETECT_UNKNOWN",
			Message: "Could not work out how to build or start this project.",
			Hint:    "Add a Dockerfile, or fill in services.web in anyship.yaml. Auto-detection covers JavaScript, Go, Python and Rust.",
		})
		svc = &spec.Service{Kind: spec.KindServer}
	}
	if err != nil {
		return nil, err
	}
	if svc.Kind != spec.KindStatic && p.has("Dockerfile") && svc.Dockerfile == "" {
		d.Evidence = append(d.Evidence, `Dockerfile found; set services.web.dockerfile to "Dockerfile" to build with it on container targets`)
	}
	d.Spec.Services = map[string]*spec.Service{ServiceName: svc}
	return d, nil
}

func (d *Detection) detectDockerfile(p project) *spec.Service {
	d.Evidence = append(d.Evidence, "Dockerfile found → container service built from it")
	svc := &spec.Service{Kind: spec.KindServer, Dockerfile: "Dockerfile"}
	if m := exposeRe.FindStringSubmatch(p.read("Dockerfile")); m != nil {
		port, _ := strconv.Atoi(m[1])
		d.Evidence = append(d.Evidence, fmt.Sprintf("Dockerfile EXPOSE %d", port))
		svc.Ports = []spec.Port{{Port: port}}
	}
	return svc
}

var exposeRe = regexp.MustCompile(`(?im)^\s*EXPOSE\s+(\d+)`)

// setName uses a manifest's project name for the spec when it yields a valid one.
func (d *Detection) setName(raw string) {
	if raw != "" {
		d.Spec.Name = specName(raw)
	}
}

// addResource records a resource the service binds to, once per name.
func (d *Detection) addResource(svc *spec.Service, typ spec.ResourceType, because string) {
	name := resourceNames[typ]
	if d.Spec.Resources[name] != nil {
		return
	}
	d.Spec.Resources[name] = &spec.Resource{Type: typ}
	svc.Uses = append(svc.Uses, name)
	d.Evidence = append(d.Evidence, fmt.Sprintf("%s → %s resource %q", because, typ, name))
}

var resourceNames = map[spec.ResourceType]string{
	spec.ResourcePostgres: "db",
	spec.ResourceMySQL:    "db",
	spec.ResourceSQLite:   "db",
	spec.ResourceRedis:    "cache",
	spec.ResourceBucket:   "storage",
	spec.ResourceKV:       "kv",
}

// listenAddress is a host:port a program binds to, found in its source.
type listenAddress struct {
	host string
	port int
	file string
}

// findListenAddress returns the first address matched by re (capture groups:
// host, port) in the project's source files with the given extensions.
func findListenAddress(p project, re *regexp.Regexp, exts ...string) (listenAddress, bool) {
	for _, file := range p.sourceFiles(exts...) {
		m := re.FindStringSubmatch(p.read(file))
		if m == nil {
			continue
		}
		port, err := strconv.Atoi(m[2])
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		return listenAddress{host: m[1], port: port, file: file}, true
	}
	return listenAddress{}, false
}

// usePort sets the service port from a source address, or assumes fallback.
// Loopback binds get a warning: inside a container they're unreachable.
func (d *Detection) usePort(svc *spec.Service, addr listenAddress, found bool, fallback int) {
	if !found {
		svc.Ports = []spec.Port{{Port: fallback}}
		d.Findings = append(d.Findings, adapter.Finding{
			Level: adapter.Info, Code: "DETECT_PORT_ASSUMED", Service: ServiceName,
			Message: fmt.Sprintf("No listen port found; assumed %d.", fallback),
		})
		return
	}
	svc.Ports = []spec.Port{{Port: addr.port}}
	d.Evidence = append(d.Evidence, fmt.Sprintf("port %d from %s", addr.port, addr.file))
	if isLoopback(addr.host) {
		d.Findings = append(d.Findings, adapter.Finding{
			Level: adapter.Warning, Code: "DETECT_LOOPBACK_BIND", Service: ServiceName, File: addr.file,
			Message: fmt.Sprintf("Listens on %s:%d, which is unreachable from outside a container.", addr.host, addr.port),
			Hint:    "Listen on 0.0.0.0 (or read the host from an environment variable).",
		})
	}
}

func isLoopback(host string) bool {
	return host == "localhost" || strings.HasPrefix(host, "127.") || host == "[::1]" || host == "::1"
}

// project gives detectors read-only access to a source tree.
type project struct{ dir string }

func (p project) has(name string) bool {
	_, err := os.Stat(filepath.Join(p.dir, name))
	return err == nil
}

// read returns a file's contents, or "" if it can't be read.
func (p project) read(name string) string {
	data, err := os.ReadFile(filepath.Join(p.dir, name))
	if err != nil {
		return ""
	}
	return string(data)
}

const maxScannedFiles = 2000

var skipDirs = []string{"node_modules", "dist", "build", "coverage", "vendor", "target", "venv", "__pycache__", "site-packages"}

var errScanLimit = errors.New("scan limit reached")

// sourceFiles lists files with the given extensions in lexical order,
// relative to the project root, skipping dependencies, build output and
// hidden directories.
func (p project) sourceFiles(exts ...string) []string {
	var out []string
	_ = filepath.WalkDir(p.dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if path != p.dir && (strings.HasPrefix(entry.Name(), ".") || slices.Contains(skipDirs, entry.Name())) {
				return filepath.SkipDir
			}
			return nil
		}
		if slices.Contains(exts, filepath.Ext(path)) && !strings.HasSuffix(path, ".d.ts") {
			rel, err := filepath.Rel(p.dir, path)
			if err == nil {
				out = append(out, filepath.ToSlash(rel))
			}
			if len(out) >= maxScannedFiles {
				return errScanLimit
			}
		}
		return nil
	})
	return out
}

var (
	scopeRe   = regexp.MustCompile(`^@[^/]+/`)
	invalidRe = regexp.MustCompile(`[^a-z0-9-]+`)
)

// specName turns a package, module or directory name into a valid spec name.
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

func ptr[T any](v T) *T { return &v }
