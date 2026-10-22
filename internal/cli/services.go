package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/spec"
)

// addServiceFlag adds --service, which deploys some services only.
func addServiceFlag(cmd *cobra.Command, services *[]string) {
	cmd.Flags().StringArrayVar(services, "service", nil,
		"deploy only this service and leave the others as they run (repeatable)")
}

// selectServices checks --service against the spec, the target and the
// --image flags, and returns the selection for Env.Services: nil, for every
// service, when no name is given.
func selectServices(s *spec.Spec, ad adapter.Adapter, names []string, images map[string]string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, name := range names {
		if _, ok := s.Services[name]; !ok {
			return nil, fmt.Errorf("unknown service %q; services: %s", name, strings.Join(s.ServiceNames(), ", "))
		}
		// This refusal is reworded for MCP agents by agentText in internal/cli/mcp.go; keep in sync.
		if seen[name] {
			return nil, fmt.Errorf("--service names %s twice", name)
		}
		seen[name] = true
	}
	// This refusal is reworded for MCP agents by agentText in internal/cli/mcp.go; keep in sync.
	if _, ok := ad.(adapter.ServiceSelector); !ok {
		return nil, fmt.Errorf("the %s target can't deploy some services only; drop --service to deploy all of them", ad.Name())
	}
	for _, name := range slices.Sorted(maps.Keys(images)) {
		// This refusal is reworded for MCP agents by agentText in internal/cli/mcp.go; keep in sync.
		if !seen[name] {
			return nil, fmt.Errorf("--image %s is for a service this run leaves out; add --service %s or drop the image", name, name)
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}
