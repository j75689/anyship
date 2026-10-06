package spec

import (
	"regexp"
	"slices"
)

// serviceURLRe matches a reference to another service's URL in an env
// value: ${services.<name>.url}. Each target resolves it to the address
// the service has there, so a spec never carries a platform's own URL.
var serviceURLRe = regexp.MustCompile(`\$\{services\.([a-z][a-z0-9-]*)\.url\}`)

// ServiceRefs lists the services an env value refers to.
func ServiceRefs(value string) []string {
	var names []string
	for _, m := range serviceURLRe.FindAllStringSubmatch(value, -1) {
		names = append(names, m[1])
	}
	return names
}

// ExpandServiceURLs replaces every reference in an env value with the URL
// the function gives for the service.
func ExpandServiceURLs(value string, url func(service string) string) string {
	return serviceURLRe.ReplaceAllStringFunc(value, func(ref string) string {
		return url(serviceURLRe.FindStringSubmatch(ref)[1])
	})
}

// RefersTo names the services the service's env refers to, each once, in
// name order.
func (s *Service) RefersTo() []string {
	var names []string
	for _, value := range s.Env {
		names = append(names, ServiceRefs(value)...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}
