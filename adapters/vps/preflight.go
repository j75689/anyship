package vps

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

// preflightPrefix marks the machine-readable lines of the preflight script;
// everything else it prints (like Compose's own errors) is for the user.
const preflightPrefix = "ANYSHIP "

// preflightScript checks the host before anything is uploaded or started.
// It reads compose.yaml from stdin into a temporary directory, which is
// removed on exit, and reports one fact per ANYSHIP line.
func preflightScript(d *planData, docker string) string {
	compose := fmt.Sprintf(`%s compose -p %s -f "$tmp/compose.yaml"`, docker, d.project)
	var b strings.Builder
	b.WriteString("tmp=$(mktemp -d) || exit 1\ntrap 'rm -rf \"$tmp\"' EXIT\ncat > \"$tmp/compose.yaml\"\nmkdir -p \"$tmp/secrets\"\n")
	// Compose resolves secret files and build contexts relative to the file.
	for _, name := range append(append([]string{}, d.generated...), d.required...) {
		fmt.Fprintf(&b, "touch \"$tmp/secrets/%s\"\n", name)
	}
	for _, service := range sortedKeys(d.contexts) {
		fmt.Fprintf(&b, "mkdir -p \"$tmp/%s\"\n", contextDir(service))
	}
	fmt.Fprintf(&b, "echo \"ANYSHIP version $(%s compose version --short 2>/dev/null)\"\n", docker)
	fmt.Fprintf(&b, "if %s config --quiet; then echo 'ANYSHIP compose ok'; else echo 'ANYSHIP compose fail'; fi\n", compose)
	b.WriteString("echo \"ANYSHIP arch $(uname -m)\"\n")
	fmt.Fprintf(&b, "root=$(%s info --format '{{.DockerRootDir}}' 2>/dev/null); root=${root:-/}\n", docker)
	b.WriteString("echo \"ANYSHIP disk $root $(df -Pk \"$root\" 2>/dev/null | awk 'NR==2 {print $4}')\"\n")
	fmt.Fprintf(&b, "echo \"ANYSHIP running $(%s ps -q 2>/dev/null | wc -l)\"\n", compose)
	b.WriteString("if command -v ss >/dev/null 2>&1; then ss -Htlnu | awk '{print \"ANYSHIP listen\", $1, $5}'; else echo 'ANYSHIP listen unavailable'; fi\n")
	return b.String()
}

// preflight holds what the script reported. Zero values mean "not reported".
type preflight struct {
	composeVersion string
	composeChecked bool
	composeOK      bool
	arch           string
	diskPath       string
	diskFreeKB     int64
	running        int
	runningKnown   bool
	listening      map[string]bool // "80/tcp"
	listenKnown    bool
}

func parsePreflight(out []byte) preflight {
	p := preflight{listening: map[string]bool{}, diskFreeKB: -1}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), preflightPrefix)
		if !ok {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "version":
			if len(fields) > 1 {
				p.composeVersion = fields[1]
			}
		case "compose":
			p.composeChecked = len(fields) > 1
			p.composeOK = len(fields) > 1 && fields[1] == "ok"
		case "arch":
			if len(fields) > 1 {
				p.arch = fields[1]
			}
		case "disk":
			if len(fields) > 1 {
				p.diskPath = fields[1]
			}
			if len(fields) > 2 {
				if kb, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
					p.diskFreeKB = kb
				}
			}
		case "running":
			if len(fields) > 1 {
				if n, err := strconv.Atoi(fields[1]); err == nil {
					p.running, p.runningKnown = n, true
				}
			}
		case "listen":
			if len(fields) == 2 && fields[1] == "unavailable" {
				continue
			}
			if len(fields) == 3 {
				p.listenKnown = true
				if port := listenPort(fields[2]); port != "" {
					p.listening[port+"/"+fields[1]] = true
				}
			}
		}
	}
	return p
}

// listenPort extracts the port from an ss local address such as
// "0.0.0.0:80", "[::]:443", "*:30303" or "127.0.0.53%lo:53".
func listenPort(local string) string {
	i := strings.LastIndex(local, ":")
	if i < 0 {
		return ""
	}
	port := local[i+1:]
	if _, err := strconv.Atoi(port); err != nil {
		return ""
	}
	return port
}

// findings turns the report into findings for the spec being deployed.
func (p preflight) findings(s *spec.Spec, host string) []adapter.Finding {
	var out []adapter.Finding
	add := func(level adapter.Level, code, message, hint string) {
		out = append(out, adapter.Finding{Level: level, Code: code, Message: message, Hint: hint})
	}

	switch {
	case p.composeVersion == "":
		add(adapter.Error, "VPS_PREFLIGHT_DOCKER",
			fmt.Sprintf("Docker Compose isn't available on %s.", host),
			"Install Docker with the Compose plugin there, add the ssh user to the docker group, or set targets.vps.sudo.")
		return out // nothing else can be trusted without Docker
	case !p.composeChecked:
		add(adapter.Warning, "VPS_PREFLIGHT_INCOMPLETE", "The preflight checks stopped early; some checks were skipped.", "")
	case !p.composeOK:
		add(adapter.Error, "VPS_PREFLIGHT_COMPOSE",
			fmt.Sprintf("Docker Compose %s on %s rejected the generated compose.yaml (its error is shown above).", p.composeVersion, host),
			"Upgrade Docker Compose on the host, or report the error if it looks like an anyship bug.")
	default:
		add(adapter.Info, "VPS_PREFLIGHT_COMPOSE_OK", fmt.Sprintf("compose.yaml is valid for Docker Compose %s on %s.", p.composeVersion, host), "")
	}

	if arch := normalizeArch(p.arch); arch != "" {
		var images []string
		for _, name := range s.ServiceNames() {
			if img := s.Services[name].Image; img != "" {
				images = append(images, img)
			}
		}
		if arch == "arm64" && len(images) > 0 {
			add(adapter.Warning, "VPS_PREFLIGHT_ARCH",
				fmt.Sprintf("%s is arm64; these images must publish linux/arm64 variants: %s.", host, strings.Join(images, ", ")), "")
		} else {
			add(adapter.Info, "VPS_PREFLIGHT_ARCH", fmt.Sprintf("%s runs linux/%s.", host, arch), "")
		}
	}

	needed := volumeBytes(s)
	switch {
	case p.diskFreeKB < 0:
		add(adapter.Warning, "VPS_PREFLIGHT_DISK", fmt.Sprintf("Couldn't read the free disk space on %s.", host), "")
	case needed > p.diskFreeKB*1024:
		add(adapter.Warning, "VPS_PREFLIGHT_DISK",
			fmt.Sprintf("Volumes are sized at %s in total, but only %s is free under %s on %s.", humanBytes(needed), humanBytes(p.diskFreeKB*1024), p.diskPath, host),
			"Add disk space before the volumes fill up, or lower the sizes in anyship.yaml.")
	default:
		add(adapter.Info, "VPS_PREFLIGHT_DISK", fmt.Sprintf("%s free under %s on %s.", humanBytes(p.diskFreeKB*1024), p.diskPath, host), "")
	}

	switch {
	case p.runningKnown && p.running > 0:
		add(adapter.Info, "VPS_PREFLIGHT_PORTS",
			fmt.Sprintf("%s is already running on %s, so its ports weren't checked (a redeploy reuses them).", s.Name, host), "")
	case !p.listenKnown:
		add(adapter.Warning, "VPS_PREFLIGHT_PORTS", fmt.Sprintf("Couldn't list the ports in use on %s (ss isn't installed).", host), "")
	default:
		for _, name := range s.ServiceNames() {
			for _, m := range portMappings(s.Services[name]) {
				if p.listening[m.key()] {
					out = append(out, adapter.Finding{
						Level: adapter.Error, Code: "VPS_PREFLIGHT_PORT_IN_USE", Service: name,
						Message: fmt.Sprintf("Port %s is already in use on %s.", m.key(), host),
						Hint:    "Stop whatever listens there, or publish a different port in anyship.yaml.",
					})
				}
			}
		}
	}
	return out
}

func normalizeArch(uname string) string {
	switch uname {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return uname
	}
}

var sizeRe = regexp.MustCompile(`^(\d+)(GB|TB)$`)

// volumeBytes sums the declared volume sizes, counting each volume once.
func volumeBytes(s *spec.Spec) int64 {
	seen := map[string]bool{}
	var total int64
	for _, name := range s.ServiceNames() {
		for _, v := range s.Services[name].Volumes {
			if seen[v.Name] {
				continue
			}
			seen[v.Name] = true
			if m := sizeRe.FindStringSubmatch(v.Size); m != nil {
				n, _ := strconv.ParseInt(m[1], 10, 64)
				unit := int64(1e9)
				if m[2] == "TB" {
					unit = 1e12
				}
				total += n * unit
			}
		}
	}
	return total
}

// humanBytes formats a byte count in decimal units, like disk vendors do.
func humanBytes(n int64) string {
	switch {
	case n >= 1e12:
		return strconv.FormatFloat(float64(n)/1e12, 'f', 1, 64) + " TB"
	case n >= 1e9:
		return strconv.FormatFloat(float64(n)/1e9, 'f', 1, 64) + " GB"
	default:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + " MB"
	}
}
