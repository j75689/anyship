package vps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/internal/shellwords"
	"github.com/j75689/anyship/spec"
)

var (
	_ adapter.StatusReader = (*Adapter)(nil)
	_ adapter.Destroyer    = (*Adapter)(nil)
)

// notDeployedExit is the exit status remote scripts use when the deployment
// directory or its compose.yaml doesn't exist.
const notDeployedExit = 3

// enterDeployment is a shell prefix that enters the deployment directory or
// exits with notDeployedExit.
func enterDeployment(dir string) string {
	return fmt.Sprintf("cd %s 2>/dev/null && [ -f compose.yaml ] || exit %d", shellwords.Quote(dir), notDeployedExit)
}

func notDeployed(err error) bool {
	code, ok := adapter.ExitCode(err)
	return ok && code == notDeployedExit
}

// psContainer is one entry of `docker compose ps --format json`.
type psContainer struct {
	Service    string
	State      string
	Health     string
	Status     string
	Publishers []struct {
		PublishedPort int
		Protocol      string
	}
}

// Status reports the project's containers per spec service.
func (a *Adapter) Status(ctx context.Context, s *spec.Spec, env *adapter.Env) (*adapter.Status, error) {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return nil, fmt.Errorf("targets.vps: %w", err)
	}
	dir := o.deployDir(s.Name)
	st := &adapter.Status{Target: Name, Location: o.Host + ":" + dir}

	remote := fmt.Sprintf("%s; %s compose -p %s -f compose.yaml ps --all --format json", enterDeployment(dir), o.docker(), s.Name)
	var out bytes.Buffer
	err = env.Exec(ctx, adapter.ExecOptions{Dir: env.Dir, Stdout: &out}, "ssh", append(sshArgs(*o), remote)...)
	if notDeployed(err) {
		st.Services = serviceStatuses(s, nil)
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading status from %s failed (%w)", o.Host, err)
	}
	containers, err := parsePS(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("unexpected `docker compose ps` output from %s: %w", o.Host, err)
	}
	st.Deployed = true
	st.Services = serviceStatuses(s, containers)
	return st, nil
}

// parsePS reads `docker compose ps --format json`, which newer Compose
// versions print as one object per line and older ones as an array.
func parsePS(data []byte) ([]psContainer, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	var containers []psContainer
	if data[0] == '[' {
		err := json.Unmarshal(data, &containers)
		return containers, err
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if line = bytes.TrimSpace(line); len(line) == 0 {
			continue
		}
		var c psContainer
		if err := json.Unmarshal(line, &c); err != nil {
			return nil, err
		}
		containers = append(containers, c)
	}
	return containers, nil
}

func serviceStatuses(s *spec.Spec, containers []psContainer) []adapter.ServiceStatus {
	var out []adapter.ServiceStatus
	for _, name := range s.ServiceNames() {
		ss := adapter.ServiceStatus{Name: name, Desired: s.Services[name].Replicas}
		var ports []string
		instances, notRunning := 0, ""
		for _, c := range containers {
			if c.Service != name {
				continue
			}
			instances++
			if c.State == "running" {
				ss.Running++
			} else if notRunning == "" {
				notRunning = c.State
			}
			ss.Health = worseHealth(ss.Health, c.Health)
			if ss.Detail == "" {
				ss.Detail = c.Status
			}
			for _, p := range c.Publishers {
				if p.PublishedPort > 0 {
					ports = append(ports, fmt.Sprintf("%d/%s", p.PublishedPort, p.Protocol))
				}
			}
		}
		switch {
		case instances == 0:
			ss.State = "missing"
		case notRunning != "":
			ss.State = notRunning
		default:
			ss.State = "running"
		}
		slices.Sort(ports)
		ss.Ports = slices.Compact(ports) // IPv4 and IPv6 bindings list each port twice
		out = append(out, ss)
	}
	return out
}

// worseHealth keeps the most alarming health of a service's instances.
func worseHealth(a, b string) string {
	rank := map[string]int{"": 0, "healthy": 1, "starting": 2, "unhealthy": 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func (a *Adapter) DestroySummary(s *spec.Spec, opts adapter.DestroyOptions) ([]string, error) {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return nil, fmt.Errorf("targets.vps: %w", err)
	}
	dir := o.deployDir(s.Name)
	lines := []string{fmt.Sprintf("Stop and remove the %s containers and network on %s.", s.Name, o.Host)}
	volumes := volumeNames(s)
	switch {
	case opts.Volumes && len(volumes) > 0:
		lines = append(lines, fmt.Sprintf("DELETE the volumes %s and %s:%s, including secrets. Their data cannot be recovered.", strings.Join(volumes, ", "), o.Host, dir))
	case opts.Volumes:
		lines = append(lines, fmt.Sprintf("DELETE %s:%s, including secrets.", o.Host, dir))
	case len(volumes) > 0:
		lines = append(lines, fmt.Sprintf("Keep the volumes %s and the secrets in %s:%s; destroy --volumes deletes them.", strings.Join(volumes, ", "), o.Host, dir))
	default:
		lines = append(lines, fmt.Sprintf("Keep the secrets and files in %s:%s; destroy --volumes deletes them.", o.Host, dir))
	}
	return lines, nil
}

// Destroy runs `docker compose down`, and with opts.Volumes also removes the
// project's volumes and deployment directory.
func (a *Adapter) Destroy(ctx context.Context, s *spec.Spec, env *adapter.Env, opts adapter.DestroyOptions) (*adapter.Result, error) {
	o, err := decodeOptions(s.Targets[Name])
	if err != nil {
		return nil, fmt.Errorf("targets.vps: %w", err)
	}
	dir := o.deployDir(s.Name)
	down := fmt.Sprintf("%s compose -p %s -f compose.yaml down --remove-orphans", o.docker(), s.Name)
	if opts.Volumes {
		down += " --volumes"
	}
	script := fmt.Sprintf("set -eu\n%s\n%s\n", enterDeployment(dir), down)
	if opts.Volumes {
		// decodeOptions guarantees dir is a subdirectory anyship owns.
		script += fmt.Sprintf("cd && rm -rf %s\n", shellwords.Quote(dir))
	}

	env.Logf("$ ssh %s %s", o.Host, down)
	err = env.Exec(ctx, adapter.ExecOptions{Dir: env.Dir}, "ssh", append(sshArgs(*o), script)...)
	switch {
	case notDeployed(err):
		return &adapter.Result{OK: true, Messages: []string{fmt.Sprintf("Nothing to remove: %s is not deployed at %s:%s.", s.Name, o.Host, dir)}}, nil
	case err != nil:
		return failed(fmt.Sprintf("docker compose down failed on %s: %v", o.Host, err)), nil
	case opts.Volumes:
		return &adapter.Result{OK: true, Messages: []string{fmt.Sprintf("Removed %s and its data from %s.", s.Name, o.Host)}}, nil
	default:
		return &adapter.Result{OK: true, Messages: []string{fmt.Sprintf("Removed %s from %s; volumes and secrets were kept.", s.Name, o.Host)}}, nil
	}
}

func volumeNames(s *spec.Spec) []string {
	var names []string
	for _, name := range s.ServiceNames() {
		for _, v := range s.Services[name].Volumes {
			names = append(names, v.Name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}
