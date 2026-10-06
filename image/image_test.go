package image

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/dockerfile"
)

// fakeBuildx writes metadata to the --metadata-file path like buildx does.
func fakeBuildx(metadata string, args *[]string) func(context.Context, adapter.ExecOptions, string, ...string) error {
	return func(_ context.Context, _ adapter.ExecOptions, _ string, a ...string) error {
		*args = a
		i := slices.Index(a, "--metadata-file")
		return os.WriteFile(a[i+1], []byte(metadata), 0o644)
	}
}

func TestBuildAndPush(t *testing.T) {
	var args []string
	digest := "sha256:" + strings.Repeat("b", 64)
	env := &adapter.Env{Logf: func(string, ...any) {}, Exec: fakeBuildx(`{"containerimage.digest": "`+digest+`"}`, &args)}
	ref, err := BuildAndPush(context.Background(), env, Build{Context: "/src", Dockerfile: "/src/Dockerfile", Repository: "reg/p/r/app"})
	if err != nil {
		t.Fatal(err)
	}
	if ref != "reg/p/r/app@"+digest {
		t.Errorf("ref = %q", ref)
	}
	got := strings.Join(args, " ")
	for _, want := range []string{"buildx build --platform linux/amd64 --file /src/Dockerfile --tag reg/p/r/app:latest", "--push /src"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q lack %q", got, want)
		}
	}
	metadata := args[slices.Index(args, "--metadata-file")+1]
	if _, err := os.Stat(filepath.Dir(metadata)); !os.IsNotExist(err) {
		t.Error("temporary metadata directory was not removed")
	}
}

func TestBuildAndPushWithoutDigest(t *testing.T) {
	var args []string
	env := &adapter.Env{Logf: func(string, ...any) {}, Exec: fakeBuildx(`{}`, &args)}
	if _, err := BuildAndPush(context.Background(), env, Build{Context: ".", Dockerfile: "Dockerfile", Repository: "r"}); err == nil {
		t.Error("expected an error when buildx reports no digest")
	}
}

func TestWriteGenerated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	path, err := WriteGenerated(dir, "web", &dockerfile.Result{Dockerfile: []byte("FROM scratch\n"), Ignore: []byte("node_modules\n")})
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "web.Dockerfile") {
		t.Errorf("path = %q", path)
	}
	if data, err := os.ReadFile(path + ".dockerignore"); err != nil || string(data) != "node_modules\n" {
		t.Errorf("ignore file = %q, %v", data, err)
	}
}

func TestMovingTag(t *testing.T) {
	for ref, want := range map[string]string{
		"nginx":                  "latest",
		"nginx:latest":           "latest",
		"ghcr.io/acme/shop:main": "main",
		// The colon here is the registry's port, not a tag.
		"localhost:5000/shop":                     "latest",
		"localhost:5000/shop:stable":              "stable",
		"us-docker.pkg.dev/p/remote/acme/shop:pr": "pr",
	} {
		if tag, moving := MovingTag(ref); !moving || tag != want {
			t.Errorf("MovingTag(%q) = %q, %v; want %q, true", ref, tag, moving, want)
		}
	}
	for _, ref := range []string{
		"nginx:1.27",
		"nginx:1.27-alpine",
		"ghcr.io/acme/shop:v2",
		"ghcr.io/acme/shop:sha-3f2a1c9",
		"ghcr.io/acme/shop:2026-10-06",
		"ghcr.io/acme/shop@sha256:" + strings.Repeat("a", 64),
		"ghcr.io/acme/shop:main@sha256:" + strings.Repeat("a", 64),
		"localhost:5000/shop:1",
	} {
		if tag, moving := MovingTag(ref); moving {
			t.Errorf("MovingTag(%q) = %q, true; want it to count as pinned", ref, tag)
		}
	}
}
