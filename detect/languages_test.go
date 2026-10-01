package detect

import (
	"slices"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

func web(d *Detection) *spec.Service { return d.Spec.Services[ServiceName] }

func wantStart(t *testing.T, d *Detection, build, start string, port int) {
	t.Helper()
	svc := web(d)
	gotBuild := ""
	if svc.Build != nil {
		gotBuild = svc.Build.Command
	}
	if gotBuild != build || svc.Start != start {
		t.Errorf("build/start = %q / %q, want %q / %q (findings %v)", gotBuild, svc.Start, build, start, codes(d.Findings))
	}
	if len(svc.Ports) != 1 || svc.Ports[0].Port != port {
		t.Errorf("ports = %+v, want %d", svc.Ports, port)
	}
	if adapter.HasErrors(d.Findings) {
		t.Errorf("unexpected errors: %+v", d.Findings)
	}
	if err := reparse(t, d.Spec); err != nil {
		t.Errorf("draft should be a valid spec: %v", err)
	}
}

func TestGoModuleRoot(t *testing.T) {
	d := detect(t, map[string]string{
		"go.mod":  "module github.com/acme/hello-api\n\ngo 1.24\n\nrequire github.com/gin-gonic/gin v1.10.0\nrequire github.com/jackc/pgx/v5 v5.7.0\n",
		"main.go": "package main\n\nfunc main() { r := gin.Default(); r.Run(\":9090\") }\n",
	})
	wantStart(t, d, "go build -o bin/hello-api .", "./bin/hello-api", 9090)
	if d.Spec.Name != "hello-api" || web(d).Framework() != "gin" || web(d).Runtime.Language != "go" {
		t.Errorf("name/framework/language = %q %q %q", d.Spec.Name, web(d).Framework(), web(d).Runtime.Language)
	}
	if d.Spec.Resources["db"] == nil || d.Spec.Resources["db"].Type != spec.ResourcePostgres {
		t.Errorf("resources = %+v", d.Spec.Resources)
	}
}

func TestGoCommandsUnderCmd(t *testing.T) {
	d := detect(t, map[string]string{
		"go.mod":              "module example.com/svc\n",
		"cmd/worker/main.go":  "package main\nfunc main() {}\n",
		"cmd/server/main.go":  "package main\nimport \"net/http\"\nfunc main() { http.ListenAndServe(\"127.0.0.1:8081\", nil) }\n",
		"internal/lib/lib.go": "package lib\n",
	})
	wantStart(t, d, "go build -o bin/server ./cmd/server", "./bin/server", 8081)
	got := codes(d.Findings)
	for _, want := range []string{"DETECT_GO_MULTIPLE_COMMANDS", "DETECT_LOOPBACK_BIND"} {
		if !slices.Contains(got, want) {
			t.Errorf("findings %v missing %s", got, want)
		}
	}
}

func TestGoWithoutMainPackage(t *testing.T) {
	d := detect(t, map[string]string{"go.mod": "module example.com/lib\n", "lib.go": "package lib\n"})
	if !slices.Contains(codes(d.Findings), "DETECT_NO_START") {
		t.Errorf("findings = %v", codes(d.Findings))
	}
}

func TestRustBinary(t *testing.T) {
	d := detect(t, map[string]string{
		"Cargo.toml":  "[package]\nname = \"edge-proxy\"\nversion = \"0.1.0\"\n\n[dependencies]\naxum = \"0.8\"\ntokio = { version = \"1\", features = [\"full\"] }\nredis = \"0.27\"\n",
		"src/main.rs": "let listener = TcpListener::bind(\"0.0.0.0:3001\").await?;\n",
	})
	wantStart(t, d, "cargo build --release", "./target/release/edge-proxy", 3001)
	if web(d).Framework() != "axum" || d.Spec.Resources["cache"] == nil {
		t.Errorf("framework = %q, resources = %+v", web(d).Framework(), d.Spec.Resources)
	}
}

func TestRustRocketListensOnAllInterfaces(t *testing.T) {
	d := detect(t, map[string]string{
		"Cargo.toml":  "[package]\nname = \"site\"\n\n[[bin]]\nname = \"site-server\"\npath = \"src/main.rs\"\n\n[dependencies]\nrocket = \"0.5\"\n",
		"src/main.rs": "fn main() {}\n",
	})
	wantStart(t, d, "cargo build --release", "./target/release/site-server", 8000)
	if web(d).Env["ROCKET_ADDRESS"] != "0.0.0.0" {
		t.Errorf("env = %v", web(d).Env)
	}
}

func TestRustWorkspaceIsReported(t *testing.T) {
	d := detect(t, map[string]string{"Cargo.toml": "[workspace]\nmembers = [\"a\", \"b\"]\n"})
	if !slices.Contains(codes(d.Findings), "DETECT_RUST_WORKSPACE") {
		t.Errorf("findings = %v", codes(d.Findings))
	}
}

func TestPythonFastAPI(t *testing.T) {
	d := detect(t, map[string]string{
		"requirements.txt": "fastapi==0.115.0\nuvicorn[standard]>=0.30\npsycopg[binary]\n",
		"app/main.py":      "from fastapi import FastAPI\n\napi = FastAPI()\n",
	})
	wantStart(t, d, "", "uvicorn app.main:api --host 0.0.0.0 --port 8000", 8000)
	if web(d).Framework() != "fastapi" || d.Spec.Resources["db"] == nil {
		t.Errorf("framework = %q, resources = %+v", web(d).Framework(), d.Spec.Resources)
	}
}

func TestPythonFlaskPrefersGunicorn(t *testing.T) {
	withGunicorn := detect(t, map[string]string{
		"requirements.txt": "Flask\ngunicorn\n",
		"app.py":           "from flask import Flask\napp = Flask(__name__)\n",
	})
	wantStart(t, withGunicorn, "", "gunicorn app:app --bind 0.0.0.0:8000", 8000)

	devOnly := detect(t, map[string]string{
		"pyproject.toml": "[project]\nname = \"blog\"\ndependencies = [\"flask>=3\"]\n",
		"blog.py":        "from flask import Flask\napp = Flask(__name__)\n",
	})
	if web(devOnly).Start != "flask --app blog:app run --host 0.0.0.0 --port 8000" || devOnly.Spec.Name != "blog" {
		t.Errorf("start = %q, name = %q", web(devOnly).Start, devOnly.Spec.Name)
	}
	if !slices.Contains(codes(devOnly.Findings), "DETECT_DEV_SERVER") {
		t.Errorf("findings = %v", codes(devOnly.Findings))
	}
}

func TestPythonDjango(t *testing.T) {
	d := detect(t, map[string]string{
		"requirements.txt": "Django>=5\ngunicorn\n",
		"manage.py":        "#!/usr/bin/env python\n",
		"mysite/wsgi.py":   "application = get_wsgi_application()\n",
		"blog/views.py":    "",
	})
	wantStart(t, d, "", "gunicorn mysite.wsgi --bind 0.0.0.0:8000", 8000)
}

func TestPythonScriptWithLoopbackHost(t *testing.T) {
	d := detect(t, map[string]string{
		"requirements.txt": "aiohttp\n",
		"main.py":          "web.run_app(app)\napp.run(host=\"127.0.0.1\", port=5050)\n",
	})
	if web(d).Start != "python main.py" || web(d).Ports[0].Port != 5050 {
		t.Errorf("service = %+v", web(d))
	}
	if !slices.Contains(codes(d.Findings), "DETECT_LOOPBACK_BIND") {
		t.Errorf("findings = %v", codes(d.Findings))
	}
}

func TestPythonRequires(t *testing.T) {
	manifest := "Django>=5\nredis[hiredis]\n# celery\n" + `dependencies = ["httpx", "boto3==1.34"]`
	for pkg, want := range map[string]bool{"django": true, "redis": true, "httpx": true, "boto3": true, "celery": false, "flask": false} {
		if got := pythonRequires(manifest, pkg); got != want {
			t.Errorf("pythonRequires(%q) = %v, want %v", pkg, got, want)
		}
	}
}
