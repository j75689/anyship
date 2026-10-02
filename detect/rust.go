package detect

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

var rustFrameworks = []string{"actix-web", "axum", "rocket", "warp", "poem"}

var rustResourceCrates = []struct {
	crate string
	typ   spec.ResourceType
}{
	{"tokio-postgres", spec.ResourcePostgres},
	{"postgres", spec.ResourcePostgres},
	{"mysql_async", spec.ResourceMySQL},
	{"mysql", spec.ResourceMySQL},
	{"rusqlite", spec.ResourceSQLite},
	{"redis", spec.ResourceRedis},
	{"aws-sdk-s3", spec.ResourceBucket},
}

var (
	tomlNameRe = regexp.MustCompile(`(?m)^\s*name\s*=\s*"([^"]+)"`)
	// "0.0.0.0:8080", "127.0.0.1:3000", "[::]:8080", ("0.0.0.0", 8080)
	rustListenRe = regexp.MustCompile(`"(0\.0\.0\.0|127\.0\.0\.1|localhost|\[::\]|\[::1\]|::):(\d{2,5})"|\(\s*"(0\.0\.0\.0|127\.0\.0\.1|localhost)"\s*,\s*(\d{2,5})\s*\)`)
)

func (d *Detection) detectRust(p project) *spec.Service {
	cargo := p.read("Cargo.toml")
	svc := &spec.Service{Kind: spec.KindServer, Runtime: &spec.Runtime{Language: "rust"}}

	pkgSection := tomlSection(cargo, "package")
	name := ""
	if m := tomlNameRe.FindStringSubmatch(pkgSection); m != nil {
		name = m[1]
		d.setName(name)
	}
	binary := name
	if m := tomlNameRe.FindStringSubmatch(tomlSection(cargo, "[bin]")); m != nil {
		binary = m[1]
	}

	switch {
	case pkgSection == "" && strings.Contains(cargo, "[workspace]"):
		d.Findings = append(d.Findings, adapter.Finding{
			Level: adapter.Error, Code: "DETECT_RUST_WORKSPACE", Service: ServiceName,
			Message: "This is a Cargo workspace; workspaces aren't auto-detected yet.",
			Hint:    "Run `anyship init` in the member crate to deploy, or set services.web.path to it.",
		})
		return svc
	case binary == "" || (!p.has("src/main.rs") && !strings.Contains(cargo, "[[bin]]")):
		d.Findings = append(d.Findings, adapter.Finding{
			Level: adapter.Error, Code: "DETECT_NO_START", Service: ServiceName,
			Message: "No binary target found (src/main.rs or [[bin]]).",
			Hint:    "Set services.web.build.command and services.web.start in anyship.yaml.",
		})
		return svc
	}
	d.Evidence = append(d.Evidence, fmt.Sprintf("Cargo.toml found → Rust binary %q", binary))
	svc.Build = &spec.Build{Command: "cargo build --release"}
	svc.Start = "./target/release/" + binary

	deps := tomlSection(cargo, "dependencies")
	hasCrate := func(crate string) bool {
		return regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(crate) + `\s*=`).MatchString(deps)
	}
	for _, fw := range rustFrameworks {
		if hasCrate(fw) {
			svc.Runtime.Framework = fw
			d.Evidence = append(d.Evidence, fmt.Sprintf("dependency %q → %s", fw, fw))
			break
		}
	}

	if svc.Runtime.Framework == "rocket" {
		// Rocket listens on 127.0.0.1:8000 unless told otherwise.
		svc.Env = map[string]string{"ROCKET_ADDRESS": "0.0.0.0", "ROCKET_PORT": "8000"}
		svc.Ports = []spec.Port{{Port: 8000}}
		d.Evidence = append(d.Evidence, "rocket → ROCKET_ADDRESS=0.0.0.0, port 8000")
	} else {
		addr, found := findRustListen(p)
		d.usePort(svc, addr, found, 8080)
	}

	for _, rc := range rustResourceCrates {
		if hasCrate(rc.crate) {
			d.addResource(svc, rc.typ, fmt.Sprintf("dependency %q", rc.crate))
		}
	}
	return svc
}

func findRustListen(p project) (listenAddress, bool) {
	for _, file := range p.sourceFiles(".rs") {
		m := rustListenRe.FindStringSubmatch(p.read(file))
		if m == nil {
			continue
		}
		host, port := m[1], m[2]
		if host == "" {
			host, port = m[3], m[4]
		}
		var n int
		if _, err := fmt.Sscan(port, &n); err == nil {
			return listenAddress{host: host, port: n, file: file}, true
		}
	}
	return listenAddress{}, false
}

// tomlSection returns the body of the [name] table (or the first [[name]]
// array entry when name is "[bin]"), up to the next table header.
func tomlSection(doc, name string) string {
	header := "[" + name + "]"
	lines := strings.Split(doc, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != header {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "[") {
				end = j
				break
			}
		}
		return strings.Join(lines[i+1:end], "\n")
	}
	return ""
}
