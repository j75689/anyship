// Package spec defines anyship.json: a platform-neutral description of what an
// app needs (processes, ports, disks, databases, secrets), not how a given
// platform provides it. Adapters translate it into platform config and either
// satisfy each need or report that they can't.
package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

const (
	// Version is the spec format version this package reads and writes.
	Version = 1
	// Filename is the conventional spec file name.
	Filename = "anyship.json"
)

type Spec struct {
	Schema    string               `json:"$schema,omitempty"`
	Version   int                  `json:"version"`
	Name      string               `json:"name" jsonschema_description:"Lowercase letters, digits and dashes, starting with a letter."`
	Services  map[string]*Service  `json:"services"`
	Resources map[string]*Resource `json:"resources,omitempty"`
	Secrets   map[string]*Secret   `json:"secrets,omitempty"`
	// Targets holds per-platform overrides keyed by adapter name. Each adapter
	// decodes and validates its own block.
	Targets map[string]json.RawMessage `json:"targets,omitempty" jsonschema_description:"Per-platform overrides keyed by adapter name."`
}

type ServiceKind string

const (
	// KindStatic is built files served as-is.
	KindStatic ServiceKind = "static"
	// KindServer is a process (or edge handler) that answers requests.
	KindServer ServiceKind = "server"
	// KindWorker is a long-running background process with no inbound traffic.
	KindWorker ServiceKind = "worker"
)

type Service struct {
	Kind ServiceKind `json:"kind" jsonschema:"enum=static,enum=server,enum=worker"`
	// Path is the source directory, relative to the spec file.
	Path       string `json:"path,omitempty" jsonschema_description:"Source directory, relative to the spec file. Defaults to \".\"."`
	Image      string `json:"image,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
	Build      *Build `json:"build,omitempty"`
	// Start is the command that starts the process.
	Start string `json:"start,omitempty" jsonschema_description:"Command that starts the process."`
	// Entry is a module exporting a fetch handler, for edge runtimes. Relative to Path.
	Entry       string            `json:"entry,omitempty" jsonschema_description:"Module exporting a fetch handler, for edge runtimes. Relative to path."`
	Ports       []Port            `json:"ports,omitempty"`
	Volumes     []Volume          `json:"volumes,omitempty"`
	Replicas    int               `json:"replicas,omitempty" jsonschema:"minimum=1"`
	Env         map[string]string `json:"env,omitempty"`
	Secrets     []string          `json:"secrets,omitempty" jsonschema_description:"Names of top-level secrets this service receives."`
	Uses        []string          `json:"uses,omitempty" jsonschema_description:"Names of top-level resources this service binds to."`
	Cron        []Cron            `json:"cron,omitempty"`
	Runtime     *Runtime          `json:"runtime,omitempty"`
	HealthCheck *HealthCheck      `json:"healthCheck,omitempty"`
	DependsOn   []string          `json:"dependsOn,omitempty"`
}

type Build struct {
	Command string `json:"command,omitempty"`
	// Output is the build output directory, relative to the service path.
	Output string `json:"output,omitempty" jsonschema_description:"Build output directory, relative to the service path."`
}

type Protocol string

const (
	// ProtocolHTTP ports can sit behind a platform's HTTP router.
	ProtocolHTTP   Protocol = "http"
	ProtocolTCP    Protocol = "tcp"
	ProtocolUDP    Protocol = "udp"
	ProtocolTCPUDP Protocol = "tcp+udp"
)

type Exposure string

const (
	ExposurePublic Exposure = "public"
	// ExposureInternal ports are reachable only by other services of the spec.
	ExposureInternal Exposure = "internal"
)

type Port struct {
	Name     string   `json:"name,omitempty"`
	Port     int      `json:"port" jsonschema:"minimum=1,maximum=65535"`
	Protocol Protocol `json:"protocol,omitempty" jsonschema:"enum=http,enum=tcp,enum=udp,enum=tcp+udp"`
	Exposure Exposure `json:"exposure,omitempty" jsonschema:"enum=public,enum=internal"`
}

type Volume struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	Size      string `json:"size" jsonschema_description:"Size like \"20GB\" or \"2TB\"."`
	Class     string `json:"class,omitempty" jsonschema:"enum=standard,enum=nvme"`
}

type Cron struct {
	Schedule string `json:"schedule"`
	// Command runs on process-based platforms. Edge platforms invoke their
	// scheduled handler instead.
	Command string `json:"command,omitempty"`
}

type Runtime struct {
	Language  string `json:"language,omitempty"`
	Framework string `json:"framework,omitempty"`
	// EdgeCompatible is false when the code relies on APIs edge runtimes
	// (e.g. Cloudflare Workers) don't provide; nil when unknown.
	EdgeCompatible *bool `json:"edgeCompatible,omitempty"`
}

type HealthCheck struct {
	Path    string `json:"path,omitempty"`
	Command string `json:"command,omitempty"`
}

type ResourceType string

const (
	ResourcePostgres ResourceType = "postgres"
	ResourceMySQL    ResourceType = "mysql"
	ResourceSQLite   ResourceType = "sqlite"
	ResourceRedis    ResourceType = "redis"
	ResourceBucket   ResourceType = "bucket"
	ResourceKV       ResourceType = "kv"
)

type Resource struct {
	Type ResourceType `json:"type" jsonschema:"enum=postgres,enum=mysql,enum=sqlite,enum=redis,enum=bucket,enum=kv"`
	// External means the resource already exists outside anyship; adapters only wire it up.
	External bool `json:"external,omitempty"`
}

type Secret struct {
	// Generate asks anyship to generate the value instead of asking for it.
	Generate    string `json:"generate,omitempty" jsonschema:"enum=hex32"`
	Description string `json:"description,omitempty"`
}

// ServiceNames returns the service names in a stable order.
func (s *Spec) ServiceNames() []string {
	return slices.Sorted(maps.Keys(s.Services))
}

// EdgeIncompatible reports whether the service is known not to run on edge runtimes.
func (svc *Service) EdgeIncompatible() bool {
	return svc.Runtime != nil && svc.Runtime.EdgeCompatible != nil && !*svc.Runtime.EdgeCompatible
}

// Framework returns the detected framework, if any.
func (svc *Service) Framework() string {
	if svc.Runtime == nil {
		return ""
	}
	return svc.Runtime.Framework
}

// Normalize fills in defaults so adapters never have to.
func (s *Spec) Normalize() {
	if s.Resources == nil {
		s.Resources = map[string]*Resource{}
	}
	if s.Secrets == nil {
		s.Secrets = map[string]*Secret{}
	}
	if s.Targets == nil {
		s.Targets = map[string]json.RawMessage{}
	}
	for _, svc := range s.Services {
		if svc == nil {
			continue
		}
		if svc.Path == "" {
			svc.Path = "."
		}
		if svc.Replicas == 0 {
			svc.Replicas = 1
		}
		if svc.Runtime == nil {
			svc.Runtime = &Runtime{}
		}
		if svc.Env == nil {
			svc.Env = map[string]string{}
		}
		for i := range svc.Ports {
			if svc.Ports[i].Protocol == "" {
				svc.Ports[i].Protocol = ProtocolHTTP
			}
			if svc.Ports[i].Exposure == "" {
				svc.Ports[i].Exposure = ExposurePublic
			}
		}
		for i := range svc.Volumes {
			if svc.Volumes[i].Class == "" {
				svc.Volumes[i].Class = "standard"
			}
		}
	}
}

// ValidationError lists every problem found in a spec.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid spec:\n  " + strings.Join(e.Problems, "\n  ")
}

// Parse decodes, normalizes and validates a spec. Unknown fields are rejected
// so typos surface instead of being ignored.
func Parse(data []byte) (*Spec, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("not a valid spec: %w", err)
	}
	if dec.More() {
		return nil, errors.New("not a valid spec: unexpected data after the top-level object")
	}
	s.Normalize()
	if problems := s.Validate(); len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	return &s, nil
}

// Load reads and parses a spec file.
func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}
