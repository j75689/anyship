package spec

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

const (
	NamePattern       = `^[a-z][a-z0-9-]{0,62}$`
	SecretNamePattern = `^[A-Z][A-Z0-9_]*$`
	SizePattern       = `^\d+(GB|TB)$`
	MemoryPattern     = `^([1-9]\d{0,5})(MB|GB)$`
	// DomainPattern matches a fully qualified domain name. It accepts either
	// case, because Normalize lowercases domains; wildcards are left out on
	// purpose, since no target attaches them.
	DomainPattern = `^(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,63}$`
	// domainMaxLength is the limit on a domain name in the DNS wire format.
	domainMaxLength = 253

	nameHint       = "use lowercase letters, digits and dashes, starting with a letter"
	secretNameHint = "use UPPER_SNAKE_CASE"
	domainHint     = `use a domain like "app.example.com"`
)

var (
	nameRe       = regexp.MustCompile(NamePattern)
	secretNameRe = regexp.MustCompile(SecretNamePattern)
	sizeRe       = regexp.MustCompile(SizePattern)
	memoryRe     = regexp.MustCompile(MemoryPattern)
	domainRe     = regexp.MustCompile(DomainPattern)

	serviceKinds  = []ServiceKind{KindStatic, KindServer, KindWorker}
	protocols     = []Protocol{ProtocolHTTP, ProtocolTCP, ProtocolUDP, ProtocolTCPUDP}
	exposures     = []Exposure{ExposurePublic, ExposureInternal}
	volumeClasses = []string{"standard", "nvme"}
	cronMethods   = []string{"GET", "POST", "PUT", "DELETE", "HEAD"}
	resourceTypes = []ResourceType{ResourcePostgres, ResourceMySQL, ResourceSQLite, ResourceRedis, ResourceBucket, ResourceKV}
)

type problems []string

// add records a problem at a dotted path such as spec.services.web.ports.0.port.
func (p *problems) add(path []any, format string, args ...any) {
	parts := make([]string, len(path))
	for i, part := range path {
		parts[i] = fmt.Sprint(part)
	}
	*p = append(*p, strings.Join(parts, ".")+": "+fmt.Sprintf(format, args...))
}

// Validate checks a normalized spec and returns every problem found, in a
// stable order. It returns nil for a valid spec.
func (s *Spec) Validate() []string {
	var p problems

	if !nameRe.MatchString(s.Name) {
		p.add([]any{"metadata", "name"}, nameHint)
	}
	if len(s.Services) == 0 {
		p.add([]any{"spec", "services"}, "at least one service is required")
	}

	for _, name := range slices.Sorted(maps.Keys(s.Resources)) {
		at := []any{"spec", "resources", name}
		if !nameRe.MatchString(name) {
			p.add(at, "invalid resource name: %s", nameHint)
		}
		r := s.Resources[name]
		if r == nil {
			p.add(at, "must be an object")
		} else if !slices.Contains(resourceTypes, r.Type) {
			p.add(append(at, "type"), "must be one of %s", quoted(resourceTypes))
		}
	}

	for _, name := range slices.Sorted(maps.Keys(s.Secrets)) {
		at := []any{"spec", "secrets", name}
		if !secretNameRe.MatchString(name) {
			p.add(at, "invalid secret name: %s", secretNameHint)
		}
		if sec := s.Secrets[name]; sec != nil && sec.Generate != "" && sec.Generate != "hex32" {
			p.add(append(at, "generate"), `must be "hex32"`)
		}
	}

	// claimedDomains maps a domain to the service that claimed it first, so a
	// domain used twice in the app is reported on its second use.
	claimedDomains := map[string]string{}
	for _, name := range s.ServiceNames() {
		s.validateService(name, claimedDomains, &p)
	}
	return p
}

func (s *Spec) validateService(name string, claimedDomains map[string]string, p *problems) {
	at := func(path ...any) []any { return append([]any{"spec", "services", name}, path...) }

	if !nameRe.MatchString(name) {
		p.add(at(), "invalid service name: %s", nameHint)
	}
	svc := s.Services[name]
	if svc == nil {
		p.add(at(), "must be an object")
		return
	}

	if !slices.Contains(serviceKinds, svc.Kind) {
		p.add(at("kind"), "must be one of %s", quoted(serviceKinds))
	}
	if svc.Replicas < 1 {
		p.add(at("replicas"), "must be at least 1")
	}

	seen := map[int]bool{}
	for i, port := range svc.Ports {
		if port.Port < 1 || port.Port > 65535 {
			p.add(at("ports", i, "port"), "must be between 1 and 65535")
		}
		if seen[port.Port] {
			p.add(at("ports", i, "port"), "port %d is listed twice", port.Port)
		}
		seen[port.Port] = true
		if !slices.Contains(protocols, port.Protocol) {
			p.add(at("ports", i, "protocol"), "must be one of %s", quoted(protocols))
		}
		if !slices.Contains(exposures, port.Exposure) {
			p.add(at("ports", i, "exposure"), "must be one of %s", quoted(exposures))
		}
	}

	for i, domain := range svc.Domains {
		switch {
		case strings.HasPrefix(domain, "*"):
			p.add(at("domains", i), "wildcard domains are not supported; name each host, e.g. \"app.example.com\"")
		case len(domain) > domainMaxLength:
			p.add(at("domains", i), "is longer than %d characters", domainMaxLength)
		case !domainRe.MatchString(domain):
			p.add(at("domains", i), "invalid domain %q: %s", domain, domainHint)
		case claimedDomains[domain] == name:
			p.add(at("domains", i), "domain %q is listed twice", domain)
		case claimedDomains[domain] != "":
			p.add(at("domains", i), "domain %q is already claimed by service %q", domain, claimedDomains[domain])
		default:
			claimedDomains[domain] = name
		}
	}
	if len(svc.Domains) > 0 && !svc.servesPublicHTTP() {
		p.add(at("domains"), "needs a port with protocol \"http\" and exposure \"public\" to route to")
	}

	for i, v := range svc.Volumes {
		if !nameRe.MatchString(v.Name) {
			p.add(at("volumes", i, "name"), nameHint)
		}
		if !strings.HasPrefix(v.MountPath, "/") {
			p.add(at("volumes", i, "mountPath"), "must be an absolute path")
		}
		if !sizeRe.MatchString(v.Size) {
			p.add(at("volumes", i, "size"), `use a size like "20GB" or "2TB"`)
		}
		if !slices.Contains(volumeClasses, v.Class) {
			p.add(at("volumes", i, "class"), "must be one of %s", quoted(volumeClasses))
		}
	}

	if svc.Memory != "" && !memoryRe.MatchString(svc.Memory) {
		p.add(at("memory"), `use a size like "512MB" or "4GB"`)
	}
	if svc.CPU < 0 {
		p.add(at("cpu"), "must be greater than 0, like 0.5, 1 or 2")
	}

	for _, key := range slices.Sorted(maps.Keys(svc.Env)) {
		for _, ref := range ServiceRefs(svc.Env[key]) {
			if _, ok := s.Services[ref]; !ok {
				p.add(at("env", key), "refers to ${services.%s.url}, but the spec has no service %q", ref, ref)
			}
		}
	}

	for i, c := range svc.Cron {
		if strings.TrimSpace(c.Schedule) == "" {
			p.add(at("cron", i, "schedule"), "is required")
		}
		switch {
		case c.Path != "" && c.Command != "":
			p.add(at("cron", i), "set command or path, not both")
		case c.Path != "" && !strings.HasPrefix(c.Path, "/"):
			p.add(at("cron", i, "path"), "must start with a slash, like \"/internal/tick\"")
		case c.Path == "" && c.Method != "":
			p.add(at("cron", i, "method"), "needs a path to call")
		}
		if c.Method != "" && !slices.Contains(cronMethods, c.Method) {
			p.add(at("cron", i, "method"), "must be one of %s", quoted(cronMethods))
		}
	}

	for i, secret := range svc.Secrets {
		if !secretNameRe.MatchString(secret) {
			p.add(at("secrets", i), secretNameHint)
		} else if _, ok := s.Secrets[secret]; !ok {
			p.add(at("secrets", i), "secret %q is not declared in spec.secrets", secret)
		}
	}
	for i, resource := range svc.Uses {
		if _, ok := s.Resources[resource]; !ok {
			p.add(at("uses", i), "unknown resource %q", resource)
		}
	}
	for i, dep := range svc.DependsOn {
		if dep == name {
			p.add(at("dependsOn", i), "a service cannot depend on itself")
		} else if _, ok := s.Services[dep]; !ok {
			p.add(at("dependsOn", i), "unknown service %q", dep)
		}
	}

	if svc.Kind == KindStatic && (svc.Image != "" || svc.Dockerfile != "") {
		p.add(at("kind"), `static services are built into files; use kind "server" to run an image`)
	}
	if svc.Kind != KindStatic && svc.Start == "" && svc.Entry == "" && svc.Image == "" && svc.Dockerfile == "" {
		p.add(at(), "needs one of start, entry, image or dockerfile")
	}
}

func quoted[T ~string](values []T) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(out, ", ")
}
