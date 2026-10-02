package detect

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

var goFrameworks = []struct{ module, name string }{
	{"github.com/gin-gonic/gin", "gin"},
	{"github.com/gofiber/fiber", "fiber"},
	{"github.com/labstack/echo", "echo"},
	{"github.com/go-chi/chi", "chi"},
}

var goResourceModules = []struct {
	module string
	typ    spec.ResourceType
}{
	{"github.com/jackc/pgx", spec.ResourcePostgres},
	{"github.com/lib/pq", spec.ResourcePostgres},
	{"github.com/go-sql-driver/mysql", spec.ResourceMySQL},
	{"github.com/mattn/go-sqlite3", spec.ResourceSQLite},
	{"modernc.org/sqlite", spec.ResourceSQLite},
	{"github.com/redis/go-redis", spec.ResourceRedis},
	{"github.com/go-redis/redis", spec.ResourceRedis},
	{"github.com/aws/aws-sdk-go-v2/service/s3", spec.ResourceBucket},
}

var (
	goModuleRe      = regexp.MustCompile(`(?m)^module\s+(\S+)`)
	goPackageMainRe = regexp.MustCompile(`(?m)^package\s+main\b`)
	// ListenAndServe(":8080"), r.Run("0.0.0.0:8080"), app.Listen(":3000"), Addr: ":8080"
	goListenRe = regexp.MustCompile(`(?:(?:ListenAndServe(?:TLS)?|Listen|Run|Start)\(\s*|Addr:\s*)"([^"]*):(\d{2,5})"`)
)

func (d *Detection) detectGo(p project) *spec.Service {
	gomod := p.read("go.mod")
	module := ""
	if m := goModuleRe.FindStringSubmatch(gomod); m != nil {
		module = m[1]
		d.setName(path.Base(module))
		d.Evidence = append(d.Evidence, "go.mod found → Go module "+module)
	}

	svc := &spec.Service{Kind: spec.KindServer, Runtime: &spec.Runtime{Language: "go"}}
	for _, f := range goFrameworks {
		if strings.Contains(gomod, f.module) {
			svc.Runtime.Framework = f.name
			d.Evidence = append(d.Evidence, fmt.Sprintf("requires %s → %s", f.module, f.name))
			break
		}
	}

	pkg, binary := d.goMainPackage(p, module)
	if pkg == "" {
		d.Findings = append(d.Findings, adapter.Finding{
			Level: adapter.Error, Code: "DETECT_NO_START", Service: ServiceName,
			Message: "No main package found at the module root or under cmd/.",
			Hint:    "Set services.web.build.command and services.web.start in anyship.yaml.",
		})
	} else {
		svc.Build = &spec.Build{Command: fmt.Sprintf("go build -o bin/%s %s", binary, pkg)}
		svc.Start = "./bin/" + binary
	}

	addr, found := findListenAddress(p, goListenRe, ".go")
	d.usePort(svc, addr, found, 8080)

	for _, rm := range goResourceModules {
		if strings.Contains(gomod, rm.module) {
			d.addResource(svc, rm.typ, "requires "+rm.module)
		}
	}
	return svc
}

// goMainPackage finds the package to build: the module root, or the single
// (or first) command under cmd/.
func (d *Detection) goMainPackage(p project, module string) (pkg, binary string) {
	binary = specName(path.Base(module))
	for _, file := range p.sourceFiles(".go") {
		if !strings.Contains(file, "/") && !strings.HasSuffix(file, "_test.go") && goPackageMainRe.MatchString(p.read(file)) {
			d.Evidence = append(d.Evidence, "main package at the module root")
			return ".", binary
		}
	}

	var commands []string
	for _, file := range p.sourceFiles(".go") {
		parts := strings.Split(file, "/")
		if len(parts) == 3 && parts[0] == "cmd" && !slices.Contains(commands, parts[1]) && goPackageMainRe.MatchString(p.read(file)) {
			commands = append(commands, parts[1])
		}
	}
	if len(commands) == 0 {
		return "", ""
	}
	if len(commands) > 1 {
		d.Findings = append(d.Findings, adapter.Finding{
			Level: adapter.Info, Code: "DETECT_GO_MULTIPLE_COMMANDS", Service: ServiceName,
			Message: fmt.Sprintf("Found commands %s; using cmd/%s.", strings.Join(commands, ", "), commands[0]),
			Hint:    "Edit services.web.build.command and start to pick another, or add a service per command.",
		})
	}
	d.Evidence = append(d.Evidence, "main package at cmd/"+commands[0])
	return "./cmd/" + commands[0], commands[0]
}
