package detect

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

var pythonResourcePackages = []struct {
	pkg string
	typ spec.ResourceType
}{
	{"psycopg", spec.ResourcePostgres},
	{"psycopg2", spec.ResourcePostgres},
	{"psycopg2-binary", spec.ResourcePostgres},
	{"asyncpg", spec.ResourcePostgres},
	{"pymysql", spec.ResourceMySQL},
	{"mysqlclient", spec.ResourceMySQL},
	{"aiomysql", spec.ResourceMySQL},
	{"redis", spec.ResourceRedis},
	{"boto3", spec.ResourceBucket},
}

var (
	fastAPIAppRe = regexp.MustCompile(`(?m)^(\w+)\s*=\s*FastAPI\(`)
	flaskAppRe   = regexp.MustCompile(`(?m)^(\w+)\s*=\s*Flask\(`)
	// app.run(host="127.0.0.1", port=5000), uvicorn.run(app, host="0.0.0.0", port=8000)
	pythonListenRe = regexp.MustCompile(`\.run\([^)]*host\s*=\s*["']([^"']+)["'][^)]*port\s*=\s*(\d{2,5})`)
	pyprojectName  = regexp.MustCompile(`(?m)^name\s*=\s*"([^"]+)"`)
)

const pythonPort = 8000

func (d *Detection) detectPython(p project) *spec.Service {
	manifest := p.read("requirements.txt") + "\n" + p.read("pyproject.toml")
	if m := pyprojectName.FindStringSubmatch(p.read("pyproject.toml")); m != nil {
		d.setName(m[1])
	}
	has := func(pkg string) bool { return pythonRequires(manifest, pkg) }
	if p.has("requirements.txt") {
		d.Evidence = append(d.Evidence, "requirements.txt found → Python project")
	} else {
		d.Evidence = append(d.Evidence, "pyproject.toml found → Python project")
	}

	svc := &spec.Service{Kind: spec.KindServer, Runtime: &spec.Runtime{Language: "python"}, Ports: []spec.Port{{Port: pythonPort}}}
	bind := fmt.Sprintf("0.0.0.0:%d", pythonPort)
	devServer := func(name string) {
		d.Findings = append(d.Findings, adapter.Finding{
			Level: adapter.Warning, Code: "DETECT_DEV_SERVER", Service: ServiceName,
			Message: fmt.Sprintf("Starting with %s's development server; it isn't meant for production.", name),
			Hint:    "Add gunicorn (or uvicorn for ASGI apps) to your dependencies and run `anyship init --force`.",
		})
	}

	switch {
	case p.has("manage.py") || has("django"):
		svc.Runtime.Framework = "django"
		module := d.findDjangoProject(p)
		switch {
		case module != "" && has("gunicorn"):
			svc.Start = fmt.Sprintf("gunicorn %s.wsgi --bind %s", module, bind)
		case p.has("manage.py"):
			svc.Start = "python manage.py runserver " + bind
			devServer("Django")
		}
	case has("fastapi"):
		svc.Runtime.Framework = "fastapi"
		if module, app := d.findPythonApp(p, fastAPIAppRe, "FastAPI"); module != "" {
			if !has("uvicorn") && !strings.Contains(manifest, "fastapi[standard]") {
				d.Findings = append(d.Findings, adapter.Finding{
					Level: adapter.Warning, Code: "DETECT_MISSING_SERVER", Service: ServiceName,
					Message: "FastAPI needs an ASGI server, but uvicorn isn't in your dependencies.",
					Hint:    `Add "uvicorn" (or "fastapi[standard]") to your dependencies.`,
				})
			}
			svc.Start = fmt.Sprintf("uvicorn %s:%s --host 0.0.0.0 --port %d", module, app, pythonPort)
		}
	case has("flask"):
		svc.Runtime.Framework = "flask"
		if module, app := d.findPythonApp(p, flaskAppRe, "Flask"); module != "" {
			if has("gunicorn") {
				svc.Start = fmt.Sprintf("gunicorn %s:%s --bind %s", module, app, bind)
			} else {
				svc.Start = fmt.Sprintf("flask --app %s:%s run --host 0.0.0.0 --port %d", module, app, pythonPort)
				devServer("Flask")
			}
		}
	default:
		for _, entry := range []string{"main.py", "app.py", "server.py"} {
			if p.has(entry) {
				svc.Start = "python " + entry
				d.Evidence = append(d.Evidence, "entry script: "+entry)
				break
			}
		}
		addr, found := findListenAddress(p, pythonListenRe, ".py")
		d.usePort(svc, addr, found, pythonPort)
	}
	if svc.Runtime.Framework != "" {
		d.Evidence = append(d.Evidence, "framework: "+svc.Runtime.Framework)
	}
	if svc.Start == "" {
		d.Findings = append(d.Findings, adapter.Finding{
			Level: adapter.Error, Code: "DETECT_NO_START", Service: ServiceName,
			Message: "Could not find how this Python app starts.",
			Hint:    "Set services.web.start in anyship.json, e.g. \"gunicorn myapp.wsgi --bind 0.0.0.0:8000\".",
		})
	}

	for _, rp := range pythonResourcePackages {
		if has(rp.pkg) {
			d.addResource(svc, rp.typ, fmt.Sprintf("dependency %q", rp.pkg))
		}
	}
	return svc
}

// findPythonApp finds the module defining the app object (e.g. app = FastAPI()).
func (d *Detection) findPythonApp(p project, re *regexp.Regexp, framework string) (module, app string) {
	for _, file := range p.sourceFiles(".py") {
		if m := re.FindStringSubmatch(p.read(file)); m != nil {
			module = strings.ReplaceAll(strings.TrimSuffix(file, ".py"), "/", ".")
			d.Evidence = append(d.Evidence, fmt.Sprintf("%s app %q in %s", framework, m[1], file))
			return module, m[1]
		}
	}
	d.Findings = append(d.Findings, adapter.Finding{
		Level: adapter.Warning, Code: "DETECT_NO_APP", Service: ServiceName,
		Message: fmt.Sprintf("Couldn't find the %s app object (like `app = %s(...)`).", framework, framework),
	})
	return "", ""
}

// findDjangoProject returns the Django project package, the one holding wsgi.py.
func (d *Detection) findDjangoProject(p project) string {
	for _, file := range p.sourceFiles(".py") {
		if path.Base(file) == "wsgi.py" && strings.Count(file, "/") == 1 {
			module := path.Dir(file)
			d.Evidence = append(d.Evidence, "Django project: "+module)
			return module
		}
	}
	return ""
}

var pythonCommentRe = regexp.MustCompile(`(?m)#.*$`)

// pythonRequires reports whether a requirements.txt or pyproject.toml text
// lists pkg as a dependency. Both formats use # comments, which are ignored.
func pythonRequires(manifest, pkg string) bool {
	re := regexp.MustCompile(`(?im)(^|["'\s,\[])` + regexp.QuoteMeta(pkg) + `($|[\s"',\[\]<>=!~;@])`)
	return re.MatchString(pythonCommentRe.ReplaceAllString(manifest, ""))
}
