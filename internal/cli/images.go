package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/j75689/anyship/spec"
)

// addImageFlag adds --image, which replaces a service's image for one run.
func addImageFlag(cmd *cobra.Command, images *[]string) {
	cmd.Flags().StringArrayVar(images, "image", nil,
		"deploy this image for a service instead of the one in the spec, as `service=image` (repeatable)")
}

// parseImages reads --image values, each "service=image".
func parseImages(values []string) (map[string]string, error) {
	images := map[string]string{}
	for _, value := range values {
		service, ref, ok := strings.Cut(value, "=")
		if !ok || service == "" || ref == "" {
			return nil, fmt.Errorf("--image %q must be service=image, such as web=ghcr.io/acme/shop@sha256:…", value)
		}
		if _, twice := images[service]; twice {
			return nil, fmt.Errorf("--image names %s twice", service)
		}
		images[service] = ref
	}
	return images, nil
}

// overrideImages replaces the image of the named services in a loaded spec.
// Nothing is written: the spec file keeps naming what it named, and the
// override lasts for this run. It is how a pipeline deploys the digest it just
// built without editing anyship.yaml, and how a tag that a registry cache
// still resolves to an old image is bypassed.
//
// Only a service that already runs an image can take another one. A service
// built from source has a build behind it that an image would silently skip.
func overrideImages(s *spec.Spec, images map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(images)) {
		ref := images[name]
		svc, ok := s.Services[name]
		switch {
		case !ok:
			return fmt.Errorf("no service %q to set an image for; services: %s", name, strings.Join(s.ServiceNames(), ", "))
		case svc.Image == "":
			return fmt.Errorf("service %q is built from source, so it has no image to replace; set services.%s.image in the spec to deploy a prebuilt one", name, name)
		case strings.ContainsAny(ref, " \t\n"):
			return fmt.Errorf("image %q for service %q is not an image reference", ref, name)
		}
		svc.Image = ref
	}
	return nil
}

// useImages applies the --image flags of a command to its spec.
func useImages(s *spec.Spec, flags []string) error {
	images, err := parseImages(flags)
	if err != nil {
		return err
	}
	return overrideImages(s, images)
}
