package spec

import (
	"regexp"
	"slices"
	"strings"
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

// A reference written almost right reaches the container as the text it is,
// and the app gets a URL it can't use. These catch the usual slips.
var (
	// bracedRefRe is ${services.<name>...} in any case, with "service" or
	// "services", any field or none, and spaces inside the braces.
	bracedRefRe = regexp.MustCompile(`(?i)\$\{\s*services?\.([^}.\s]+)(?:\.([^}\s]*))?\s*\}`)
	// bareRefRe is services.<name>.url without the braces.
	bareRefRe = regexp.MustCompile(`(?i)\bservices?\.([a-z0-9_-]+)\.url\b`)
)

// nearMiss is a piece of an env value that reads as a reference to a
// service's URL but isn't written as ${services.<name>.url}.
type nearMiss struct {
	text  string
	name  string // the service named, in lowercase
	field string // the field asked for; "url" when none other was written
}

// nearMisses finds the references in an env value that are written wrong,
// leaving the ones written right alone.
func nearMisses(value string) []nearMiss {
	rest := serviceURLRe.ReplaceAllString(value, " ")
	var out []nearMiss
	for _, m := range bracedRefRe.FindAllStringSubmatch(rest, -1) {
		field := strings.ToLower(m[2])
		if field == "" {
			field = "url"
		}
		out = append(out, nearMiss{text: m[0], name: strings.ToLower(m[1]), field: field})
	}
	rest = bracedRefRe.ReplaceAllString(rest, " ")
	for _, m := range bareRefRe.FindAllStringSubmatch(rest, -1) {
		out = append(out, nearMiss{text: m[0], name: strings.ToLower(m[1]), field: "url"})
	}
	return out
}
