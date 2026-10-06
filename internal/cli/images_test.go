package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/j75689/anyship/spec"
)

const digest = "ghcr.io/acme/shop@sha256:0000000000000000000000000000000000000000000000000000000000000000"

func imageSpec(t *testing.T) *spec.Spec {
	t.Helper()
	s, err := spec.Parse([]byte(`apiVersion: anyship/v1alpha1
kind: App
metadata:
  name: shop
spec:
  services:
    web:
      kind: server
      image: ghcr.io/acme/shop:main
      ports:
        - port: 8080
    api:
      kind: server
      start: ./api
`))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestImageFlagReplacesTheImageForThisRun(t *testing.T) {
	s := imageSpec(t)
	if err := useImages(s, []string{"web=" + digest}); err != nil {
		t.Fatal(err)
	}
	if got := s.Services["web"].Image; got != digest {
		t.Errorf("image = %q, want %q", got, digest)
	}
	if err := useImages(s, nil); err != nil || s.Services["web"].Image != digest {
		t.Errorf("no flags must change nothing: %v", err)
	}
}

func TestImageFlagRefusesWhatItCannotApply(t *testing.T) {
	for flag, want := range map[string]string{
		"web":                      `--image "web" must be service=image`,
		"=" + digest:               "must be service=image",
		"web=":                     "must be service=image",
		"worker=" + digest:         `no service "worker" to set an image for; services: api, web`,
		"api=" + digest:            `service "api" is built from source, so it has no image to replace`,
		"web=ghcr.io/acme/shop :1": "is not an image reference",
	} {
		s := imageSpec(t)
		err := useImages(s, []string{flag})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("--image %q: %v, want an error containing %q", flag, err, want)
		}
		if got := s.Services["web"].Image; got != "ghcr.io/acme/shop:main" {
			t.Errorf("--image %q: a refused override changed the image to %q", flag, got)
		}
	}
	if err := useImages(imageSpec(t), []string{"web=a:1", "web=a:2"}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("the same service twice: %v", err)
	}
}

// Over MCP the same override is the images argument of plan and apply, and it
// reaches what the target would deploy.
func TestMCPPlanDeploysTheImageItIsGiven(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, spec.Filename)
	body := `apiVersion: anyship/v1alpha1
kind: App
metadata:
  name: shop
spec:
  services:
    web:
      kind: server
      image: ghcr.io/acme/shop:main
      ports:
        - port: 8080
  targets:
    vps:
      host: deploy@203.0.113.10
`
	if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	h := connectMCP(t, false)
	compose := func(args map[string]any) string {
		var out planOutput
		if msg := h.call(t, "plan", args, &out); msg != "" {
			t.Fatal(msg)
		}
		for _, f := range out.Files {
			if filepath.Base(f.Path) == "compose.yaml" {
				return f.Contents
			}
		}
		t.Fatalf("no compose.yaml in %+v", out.Files)
		return ""
	}
	if got := compose(map[string]any{"config": config, "target": "vps"}); !strings.Contains(got, "ghcr.io/acme/shop:main") {
		t.Errorf("without images, the spec's image is deployed:\n%s", got)
	}
	got := compose(map[string]any{"config": config, "target": "vps", "images": map[string]any{"web": digest}})
	if !strings.Contains(got, digest) || strings.Contains(got, "shop:main") {
		t.Errorf("with images, compose.yaml should deploy %s:\n%s", digest, got)
	}
	if msg := h.call(t, "plan", map[string]any{"config": config, "target": "vps", "images": map[string]any{"worker": digest}}, nil); !strings.Contains(msg, `no service "worker"`) {
		t.Errorf("an unknown service: %q", msg)
	}
	if after, _ := os.ReadFile(config); string(after) != body {
		t.Error("the override must not be written to anyship.yaml")
	}
}
