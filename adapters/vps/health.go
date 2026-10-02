package vps

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/dockerfile"
	"github.com/j75689/anyship/internal/shellwords"
	"github.com/j75689/anyship/spec"
)

// Health check timings are fixed for now: the spec says what to check, anyship
// decides how often. They are chosen so a slow starter has 10s of grace and a
// broken service is declared unhealthy about 35s after it starts.
const (
	healthInterval    = "5s"
	healthTimeout     = "3s"
	healthRetries     = 5
	healthStartPeriod = "10s"
	// healthWaitSeconds is how long Apply waits for every container with a
	// health check to report healthy.
	healthWaitSeconds = 180
	// healthLogTail is how many log lines Apply shows for a service that did
	// not become healthy.
	healthLogTail = 30
)

// noHTTPClient is what a generated probe prints when the image has neither
// wget nor curl. Docker keeps a probe's output in the container's health log,
// so Apply can report that the check never really ran instead of passing the
// service off as healthy without saying why.
const noHTTPClient = "anyship: no wget or curl in this image, so healthCheck.path was not probed"

// healthProbePath is the path a probe requests, with the leading slash the
// spec may leave out.
func healthProbePath(path string) string {
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}

// healthURL is the URL a generated probe requests from inside the container.
func healthURL(port int, path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, healthProbePath(path))
}

// httpPort is the container port a generated probe talks to: the nginx port of
// a static site, otherwise the service's first HTTP port. Internal ports count
// too; the probe runs inside the container.
func httpPort(svc *spec.Service) (int, bool) {
	if svc.Kind == spec.KindStatic {
		return dockerfile.StaticPort, true
	}
	for _, p := range svc.Ports {
		if p.Protocol == spec.ProtocolHTTP {
			return p.Port, true
		}
	}
	return 0, false
}

// healthProbe is the shell command that requests url. Images ship either wget
// (busybox, alpine) or curl, so the probe tries both; an image with neither
// prints noHTTPClient and passes, which Apply turns into a warning rather than
// marking a working service unhealthy.
func healthProbe(url string) string {
	q := shellwords.Quote(url)
	return fmt.Sprintf("if command -v wget >/dev/null 2>&1; then wget -q -O /dev/null %[1]s; "+
		"elif command -v curl >/dev/null 2>&1; then curl -fsS -o /dev/null %[1]s; "+
		"else echo %[2]s; fi", q, shellwords.Quote(noHTTPClient))
}

// healthTest is the compose healthcheck test for a service, or nil when the
// spec asks for no check this target can run.
func healthTest(svc *spec.Service) []string {
	hc := svc.HealthCheck
	switch {
	case hc == nil:
		return nil
	case hc.Command != "":
		return []string{"CMD-SHELL", hc.Command}
	case hc.Path != "":
		port, ok := httpPort(svc)
		if !ok {
			return nil
		}
		return []string{"CMD-SHELL", healthProbe(healthURL(port, hc.Path))}
	}
	return nil
}

// healthChecked lists the services whose containers Apply waits on, in spec
// order.
func healthChecked(s *spec.Spec) []string {
	var out []string
	for _, name := range s.ServiceNames() {
		if healthTest(s.Services[name]) != nil {
			out = append(out, name)
		}
	}
	return out
}

// waitScript polls the project's containers until none is still starting, then
// reports one ANYSHIP line per container on stdout. The log tail of anything
// that is not healthy goes to stderr, so it reaches the user directly like
// Compose's own errors do.
func waitScript(d *planData, docker string) string {
	compose := fmt.Sprintf("%s compose -p %s -f compose.yaml", docker, d.project)
	// The service name comes from the label Compose puts on every container,
	// so replicas report under the service they belong to.
	format := `{{index .Config.Labels "com.docker.compose.service"}} {{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}`
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", enterDeployment(d.dir))
	fmt.Fprintf(&b, "ids() { %s ps --all -q 2>/dev/null; }\n", compose)
	fmt.Fprintf(&b, "state() { %s inspect --format %s \"$1\" 2>/dev/null; }\n", docker, shellwords.Quote(format))
	fmt.Fprintf(&b, "deadline=$(( $(date +%%s) + %d ))\n", healthWaitSeconds)
	b.WriteString("while :; do\n")
	b.WriteString("  starting=\n")
	b.WriteString("  for id in $(ids); do\n")
	b.WriteString("    case \"$(state \"$id\")\" in *' starting') starting=1 ;; esac\n")
	b.WriteString("  done\n")
	b.WriteString("  if [ -z \"$starting\" ] || [ \"$(date +%s)\" -ge \"$deadline\" ]; then break; fi\n")
	b.WriteString("  sleep 3\n")
	b.WriteString("done\n")
	b.WriteString("for id in $(ids); do\n")
	b.WriteString("  line=$(state \"$id\") || continue\n")
	b.WriteString("  [ -n \"$line\" ] || continue\n")
	b.WriteString("  echo \"ANYSHIP health $line\"\n")
	fmt.Fprintf(&b, "  if %s inspect --format '{{if .State.Health}}{{range .State.Health.Log}}{{.Output}}{{end}}{{end}}' \"$id\" 2>/dev/null | grep -q %s; then\n",
		docker, shellwords.Quote(noHTTPClient))
	b.WriteString("    echo \"ANYSHIP noprobe ${line%% *}\"\n")
	b.WriteString("  fi\n")
	b.WriteString("  case \"$line\" in *' healthy'|*' none') continue ;; esac\n")
	b.WriteString("  echo \"--- last lines from ${line%% *} ---\" >&2\n")
	fmt.Fprintf(&b, "  %s logs --tail %d \"$id\" >&2 2>&1 || true\n", docker, healthLogTail)
	b.WriteString("done\n")
	return b.String()
}

// healthReport is what waitScript reported, per service. A service with
// several replicas keeps its worst container's state.
type healthReport struct {
	// status is the container state, such as "running" or "exited".
	status map[string]string
	// health is the health status: healthy, unhealthy, starting, or none.
	health map[string]string
	// noProbe marks services whose image has no wget or curl.
	noProbe map[string]bool
}

// worstHealth keeps the most alarming health of a service's containers. It
// ranks "none" (Docker knows of no check) as worse than healthy, because an
// unchecked service is not a verified one.
func worstHealth(a, b string) string {
	rank := map[string]int{"": 0, "healthy": 1, "none": 2, "starting": 3, "unhealthy": 4}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func parseHealth(out []byte) healthReport {
	r := healthReport{status: map[string]string{}, health: map[string]string{}, noProbe: map[string]bool{}}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), reportPrefix)
		if !ok {
			continue
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) == 4 && fields[0] == "health":
			service, status, health := fields[1], fields[2], fields[3]
			if was, seen := r.status[service]; !seen || (was == "running" && status != "running") {
				r.status[service] = status
			}
			r.health[service] = worstHealth(r.health[service], health)
		case len(fields) == 2 && fields[0] == "noprobe":
			r.noProbe[fields[1]] = true
		}
	}
	return r
}

// findings judges the services Apply waited on. Only those carry findings: a
// service without a health check has nothing to report either way.
func (r healthReport) findings(services []string, host string) []adapter.Finding {
	var out []adapter.Finding
	add := func(level adapter.Level, code, service, message, hint string) {
		out = append(out, adapter.Finding{Level: level, Code: code, Service: service, Message: message, Hint: hint})
	}
	for _, name := range services {
		status, reported := r.status[name]
		health := r.health[name]
		switch {
		case !reported:
			add(adapter.Error, "VPS_HEALTH_NO_CONTAINER", name,
				fmt.Sprintf("No container is on %s after `docker compose up`.", host),
				"Run `anyship logs -t vps "+name+"` to see why it did not start.")
		case status != "running":
			add(adapter.Error, "VPS_HEALTH_NOT_RUNNING", name,
				fmt.Sprintf("The container is %s, not running; its last log lines are shown above.", status),
				"Fix what the log reports, then apply again.")
		case health == "unhealthy":
			add(adapter.Error, "VPS_HEALTH_UNHEALTHY", name,
				"The health check keeps failing; the container's last log lines are shown above.",
				"Check that the service listens on the health check's port and answers its path.")
		case health == "starting":
			add(adapter.Error, "VPS_HEALTH_TIMEOUT", name,
				fmt.Sprintf("Still starting after %ds; the container's last log lines are shown above.", healthWaitSeconds),
				"Make the service answer its health check sooner, or point healthCheck at a path that is ready earlier.")
		case health == "none":
			add(adapter.Warning, "VPS_HEALTH_NOT_CONFIGURED", name,
				"Docker reports no health check on this container, so its health was not verified.",
				"Report this as an anyship bug if the spec sets healthCheck for this service.")
		case r.noProbe[name]:
			add(adapter.Warning, "VPS_HEALTH_NO_HTTP_CLIENT", name,
				"The image has neither wget nor curl, so healthCheck.path was not probed and the container counts as healthy without being checked.",
				"Install wget or curl in the image, or set healthCheck.command to a check the image can run.")
		default:
			add(adapter.Info, "VPS_HEALTH_OK", name, fmt.Sprintf("Healthy on %s.", host), "")
		}
	}
	return out
}
