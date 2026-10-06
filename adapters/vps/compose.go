package vps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strconv"

	"github.com/j75689/anyship/dockerfile"
	"github.com/j75689/anyship/image"
	"github.com/j75689/anyship/spec"
)

// composeFile mirrors the subset of the Compose specification anyship renders.
// It is written as JSON, which is valid YAML, so no YAML encoder is needed and
// output is deterministic (encoding/json sorts map keys).
type composeFile struct {
	Name     string                    `json:"name"`
	Services map[string]composeService `json:"services"`
	Volumes  map[string]struct{}       `json:"volumes,omitempty"`
	Secrets  map[string]composeSecret  `json:"secrets,omitempty"`
}

type composeService struct {
	Image       string            `json:"image,omitempty"`
	PullPolicy  string            `json:"pull_policy,omitempty"`
	Build       *composeBuild     `json:"build,omitempty"`
	Entrypoint  []string          `json:"entrypoint,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Ports       []string          `json:"ports,omitempty"`
	Volumes     []string          `json:"volumes,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Secrets     []string          `json:"secrets,omitempty"`
	DependsOn   []string          `json:"depends_on,omitempty"`
	Healthcheck *composeHealth    `json:"healthcheck,omitempty"`
	Deploy      *composeDeploy    `json:"deploy,omitempty"`
	Restart     string            `json:"restart"`
}

type composeBuild struct {
	Context    string `json:"context"`
	Dockerfile string `json:"dockerfile,omitempty"`
}

type composeHealth struct {
	Test []string `json:"test"`
}

type composeDeploy struct {
	Replicas  int               `json:"replicas"`
	Resources *composeResources `json:"resources,omitempty"`
}

type composeResources struct {
	Limits composeLimits `json:"limits"`
}

type composeLimits struct {
	CPUs   string `json:"cpus,omitempty"`
	Memory string `json:"memory,omitempty"`
}

type composeSecret struct {
	File string `json:"file"`
}

// contextDir is where a service's build context lands on the host, relative
// to the deployment directory.
func contextDir(service string) string {
	return path.Join("src", service)
}

// portMapping is a host port published to a container port.
type portMapping struct {
	host, container int
	udp             bool
	// http marks plain-HTTP ports, which are served without TLS for now.
	http bool
}

func (m portMapping) String() string {
	if m.udp {
		return fmt.Sprintf("%d:%d/udp", m.host, m.container)
	}
	return fmt.Sprintf("%d:%d", m.host, m.container)
}

// key identifies the host port for conflict checks, e.g. "8080/tcp".
func (m portMapping) key() string {
	if m.udp {
		return fmt.Sprintf("%d/udp", m.host)
	}
	return fmt.Sprintf("%d/tcp", m.host)
}

// portMappings lists the host ports a service publishes. Internal ports are
// left out: other services reach them by name on the project network. Static
// sites are served by nginx on dockerfile.StaticPort, published on their
// public ports or on port 80.
func portMappings(svc *spec.Service) []portMapping {
	var out []portMapping
	if svc.Kind == spec.KindStatic {
		for _, p := range svc.Ports {
			if p.Exposure == spec.ExposurePublic && p.Protocol != spec.ProtocolUDP {
				out = append(out, portMapping{host: p.Port, container: dockerfile.StaticPort, http: true})
			}
		}
		if len(out) == 0 {
			out = append(out, portMapping{host: dockerfile.StaticPort, container: dockerfile.StaticPort, http: true})
		}
		return out
	}
	for _, p := range svc.Ports {
		if p.Exposure != spec.ExposurePublic {
			continue
		}
		if p.Protocol != spec.ProtocolUDP {
			out = append(out, portMapping{host: p.Port, container: p.Port, http: p.Protocol == spec.ProtocolHTTP})
		}
		if p.Protocol == spec.ProtocolUDP || p.Protocol == spec.ProtocolTCPUDP {
			out = append(out, portMapping{host: p.Port, container: p.Port, udp: true})
		}
	}
	return out
}

// serviceURL is where a service of the spec answers on the compose network:
// its name, and its first HTTP port (the static port for a static site).
// A service without a port has no address, and ok is false.
func serviceURL(name string, s *spec.Spec) (url string, ok bool) {
	svc := s.Services[name]
	if svc.Kind == spec.KindStatic {
		return fmt.Sprintf("http://%s:%d", name, dockerfile.StaticPort), true
	}
	for _, p := range svc.Ports {
		if p.Protocol == spec.ProtocolHTTP {
			return fmt.Sprintf("http://%s:%d", name, p.Port), true
		}
	}
	return "", false
}

// serviceEnv is the service's env with references to other services
// resolved to their addresses on the compose network.
func serviceEnv(svc *spec.Service, s *spec.Spec) map[string]string {
	if len(svc.RefersTo()) == 0 {
		return svc.Env
	}
	env := make(map[string]string, len(svc.Env))
	for k, v := range svc.Env {
		env[k] = spec.ExpandServiceURLs(v, func(name string) string {
			url, _ := serviceURL(name, s)
			return url
		})
	}
	return env
}

// renderService renders one spec service. env is the service's environment
// with references resolved; argv is the already-split start command, or nil
// to keep the image's own entrypoint and command; build is set for services
// built on the host.
func renderService(svc *spec.Service, env map[string]string, argv []string, build *composeBuild) composeService {
	out := composeService{
		Image:       svc.Image,
		Build:       build,
		Environment: env,
		Secrets:     svc.Secrets,
		DependsOn:   svc.DependsOn,
		Restart:     "unless-stopped",
	}
	if len(out.Environment) == 0 {
		out.Environment = nil
	}
	// Compose keeps the image the host already has, so a tag that was pushed
	// again would never reach a deployed service. An image that is not built
	// here and is named by a tag that moves is pulled on every deploy.
	if _, moving := image.MovingTag(svc.Image); svc.Image != "" && build == nil && moving {
		out.PullPolicy = "always"
	}
	if len(argv) > 0 {
		// Overriding the entrypoint also clears the image's default command,
		// so the spec's start command runs exactly as written.
		out.Entrypoint = argv[:1]
		out.Command = argv[1:]
	}
	for _, m := range portMappings(svc) {
		out.Ports = append(out.Ports, m.String())
	}
	for _, v := range svc.Volumes {
		out.Volumes = append(out.Volumes, v.Name+":"+v.MountPath)
	}
	if svc.HealthCheck != nil && svc.HealthCheck.Command != "" {
		out.Healthcheck = &composeHealth{Test: []string{"CMD-SHELL", svc.HealthCheck.Command}}
	}
	if svc.Replicas > 1 {
		out.Deploy = &composeDeploy{Replicas: svc.Replicas}
	}
	if svc.CPU != 0 || svc.Memory != "" {
		if out.Deploy == nil {
			out.Deploy = &composeDeploy{Replicas: svc.Replicas}
		}
		limits := composeLimits{}
		if svc.CPU != 0 {
			limits.CPUs = strconv.FormatFloat(svc.CPU, 'f', -1, 64)
		}
		if mb := svc.MemoryMB(); mb != 0 {
			limits.Memory = strconv.Itoa(mb) + "M"
		}
		out.Deploy.Resources = &composeResources{Limits: limits}
	}
	return out
}

func renderCompose(c composeFile) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("# Generated by anyship from anyship.yaml. Edit anyship.yaml instead; this file is overwritten.\n")
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
