// Package spec defines anyship.yaml: a platform-neutral description of what an
// app needs (processes, ports, disks, databases, secrets), not how a given
// platform provides it. Adapters translate it into platform config and either
// satisfy each need or report that they can't.
package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/j75689/anyship/internal/yamljson"
)

const (
	// APIVersion is the manifest version this package reads and writes.
	APIVersion = "anyship/v1alpha1"
	// Kind is the manifest kind for an app.
	Kind = "App"
	// Filename is the conventional spec file name.
	Filename = "anyship.yaml"
	// LegacyFilename is the spec file name anyship used up to v0.2.x.
	LegacyFilename = "anyship.json"
	// ChangelogURL is where the move from anyship.json to anyship.yaml is described.
	ChangelogURL = "https://github.com/j75689/anyship/blob/main/CHANGELOG.md"
	// SchemaURL is where editors find the JSON Schema for anyship.yaml.
	SchemaURL = "https://raw.githubusercontent.com/j75689/anyship/main/schema/anyship.schema.json"
)

// Manifest is the file format, a Kubernetes-style envelope around the app.
type Manifest struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       *Spec    `json:"spec"`
}

type Metadata struct {
	Name string `json:"name" jsonschema_description:"Lowercase letters, digits and dashes, starting with a letter."`
}

// Spec is the app: what adapters deploy. Name comes from metadata.name.
type Spec struct {
	Name      string               `json:"-"`
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
	Entry string `json:"entry,omitempty" jsonschema_description:"Module exporting a fetch handler, for edge runtimes. Relative to path."`
	Ports []Port `json:"ports,omitempty"`
	// Domains are the custom domains this service answers on. A target that
	// can't attach them says so instead of dropping them.
	Domains  []string `json:"domains,omitempty" jsonschema_description:"Custom domains this service answers on, e.g. \"app.example.com\". Needs a public http port. Wildcards are not supported."`
	Volumes  []Volume `json:"volumes,omitempty"`
	Replicas int      `json:"replicas,omitempty" jsonschema:"minimum=1"`
	// Memory and CPU are what one instance of the service needs. A target
	// that can't size a service says so instead of dropping them.
	Memory      string            `json:"memory,omitempty" jsonschema_description:"Memory one instance needs, like \"512MB\" or \"4GB\"."`
	CPU         float64           `json:"cpu,omitempty" jsonschema_description:"CPUs one instance needs, like 0.5, 1 or 2."`
	Env         map[string]string `json:"env,omitempty"`
	Secrets     []string          `json:"secrets,omitempty" jsonschema_description:"Names of top-level secrets this service receives."`
	Uses        []string          `json:"uses,omitempty" jsonschema_description:"Names of top-level resources this service binds to."`
	Cron        []Cron            `json:"cron,omitempty"`
	Runtime     *Runtime          `json:"runtime,omitempty"`
	HealthCheck *HealthCheck      `json:"healthCheck,omitempty"`
	DependsOn   []string          `json:"dependsOn,omitempty"`
}

// MemoryMB is Memory in megabytes, or 0 when the service doesn't set it.
// Sizes are binary, as container platforms count them: 1GB is 1024MB.
func (s *Service) MemoryMB() int {
	m := memoryRe.FindStringSubmatch(s.Memory)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	if m[2] == "GB" {
		n *= 1024
	}
	return n
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

// Cron is one schedule of a service. An entry names what runs when it fires:
// a command, or a path of the service to call over HTTP. An entry with
// neither fires the platform's own handler, as edge runtimes do.
type Cron struct {
	Schedule string `json:"schedule"`
	// Command runs on process-based platforms. Edge platforms invoke their
	// scheduled handler instead.
	Command string `json:"command,omitempty"`
	// Path is called on the service when the schedule fires, on platforms
	// that schedule HTTP calls.
	Path string `json:"path,omitempty" jsonschema_description:"Path of this service to call over HTTP when the schedule fires, e.g. \"/internal/tick\". Instead of command."`
	// Method is the HTTP method for Path; POST when empty.
	Method string `json:"method,omitempty" jsonschema:"enum=GET,enum=POST,enum=PUT,enum=DELETE,enum=HEAD" jsonschema_description:"HTTP method for path; POST by default."`
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

// DeployOrder lists services so each comes after the services it depends on.
// Services in a dependency cycle come last, in name order.
func (s *Spec) DeployOrder() []string {
	var order []string
	placed := map[string]bool{}
	for len(order) < len(s.Services) {
		progressed := false
		for _, name := range s.ServiceNames() {
			if placed[name] {
				continue
			}
			ready := true
			for _, dep := range s.Services[name].DependsOn {
				if !placed[dep] {
					ready = false
				}
			}
			if ready {
				order, placed[name], progressed = append(order, name), true, true
			}
		}
		if !progressed { // a dependency cycle; deploy the rest in name order
			for _, name := range s.ServiceNames() {
				if !placed[name] {
					order, placed[name] = append(order, name), true
				}
			}
		}
	}
	return order
}

// EdgeIncompatible reports whether the service is known not to run on edge runtimes.
func (svc *Service) EdgeIncompatible() bool {
	return svc.Runtime != nil && svc.Runtime.EdgeCompatible != nil && !*svc.Runtime.EdgeCompatible
}

// servesPublicHTTP reports whether the service has a port a platform's HTTP
// router can put a domain in front of.
func (svc *Service) servesPublicHTTP() bool {
	return slices.ContainsFunc(svc.Ports, func(p Port) bool {
		return p.Protocol == ProtocolHTTP && p.Exposure == ExposurePublic
	})
}

// Framework returns the detected framework, if any.
func (svc *Service) Framework() string {
	if svc.Runtime == nil {
		return ""
	}
	return svc.Runtime.Framework
}

// slashPath rewrites a path field written with Windows separators. Spec paths
// are always slash-separated, so the same spec means the same thing on every
// machine. filepath.ToSlash can't do this job: on Linux it is a no-op, which
// would leave "src\index.ts" as one long file name there and split it into two
// components on Windows.
func slashPath(p string) string { return strings.ReplaceAll(p, `\`, "/") }

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
		svc.Path = slashPath(svc.Path)
		svc.Dockerfile = slashPath(svc.Dockerfile)
		svc.Entry = slashPath(svc.Entry)
		if svc.Build != nil {
			svc.Build.Output = slashPath(svc.Build.Output)
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
		for i := range svc.Cron {
			svc.Cron[i].Method = strings.ToUpper(svc.Cron[i].Method)
			if svc.Cron[i].Path != "" && svc.Cron[i].Method == "" {
				svc.Cron[i].Method = "POST"
			}
		}
		// Domains are case-insensitive and may be written with the root dot.
		for i, domain := range svc.Domains {
			svc.Domains[i] = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
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

// Parse decodes, normalizes and validates a spec from YAML (or JSON, which
// is YAML too). Unknown fields are rejected so typos surface instead of being
// ignored.
func Parse(data []byte) (*Spec, error) {
	js, err := yamljson.ToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("not a valid spec: %w", err)
	}
	// Wrong shapes and unknown fields are reported by path, all at once. The
	// strict decoder below would refuse the same documents, but only says
	// "cannot unmarshal array into Go struct field Manifest.spec.secrets".
	if found := yamljson.Check(js, reflect.TypeFor[Manifest]()); len(found) > 0 {
		problems := make([]string, len(found))
		for i, problem := range found {
			problems[i] = problem.String()
		}
		return nil, &ValidationError{Problems: problems}
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("not a valid spec: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	var p problems
	if m.APIVersion != APIVersion {
		p.add([]any{"apiVersion"}, "must be %s", APIVersion)
	}
	if m.Kind != Kind {
		p.add([]any{"kind"}, "must be %s", Kind)
	}
	if m.Spec == nil {
		p.add([]any{"spec"}, "is required")
	}
	if len(p) > 0 {
		return nil, &ValidationError{Problems: p}
	}
	s := m.Spec
	s.Name = m.Metadata.Name
	s.Normalize()
	if problems := s.Validate(); len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	return s, nil
}

// Marshal renders a spec as anyship.yaml, with a schema hint for editors.
func Marshal(s *Spec) ([]byte, error) {
	js, err := json.Marshal(Manifest{APIVersion: APIVersion, Kind: Kind, Metadata: Metadata{Name: s.Name}, Spec: s})
	if err != nil {
		return nil, err
	}
	n, err := yamljson.FromJSON(js)
	if err != nil {
		return nil, err
	}
	out, err := yamljson.Encode(n)
	if err != nil {
		return nil, err
	}
	return append([]byte("# yaml-language-server: $schema="+SchemaURL+"\n"), out...), nil
}

// Load reads and parses a spec file.
func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && filepath.Base(path) == Filename {
		if _, jsonErr := os.Stat(filepath.Join(filepath.Dir(path), LegacyFilename)); jsonErr == nil {
			return nil, fmt.Errorf("%s not found, but %s is: specs are YAML manifests now (apiVersion: %s); the 0.3.0 notes say what moved: %s", Filename, LegacyFilename, APIVersion, ChangelogURL)
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		abs, absErr := filepath.Abs(path)
		if absErr != nil {
			abs = path
		}
		return nil, &NotFoundError{Path: abs}
	}
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// NotFoundError is what Load returns when there is no spec file. It carries
// the absolute path that was tried: "anyship.yaml" alone does not say which
// directory the caller was looking in.
type NotFoundError struct {
	Path string
}

func (e *NotFoundError) Error() string { return e.Path + " does not exist" }

func (e *NotFoundError) Unwrap() error { return fs.ErrNotExist }
