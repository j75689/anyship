// Package image builds container images and pushes them to a registry. It
// returns the pushed digest, so a deploy runs exactly the image it built.
package image

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/dockerfile"
)

// DefaultPlatform is what managed container services run.
const DefaultPlatform = "linux/amd64"

type Build struct {
	// Context is the build context directory.
	Context string
	// Dockerfile is the path of the Dockerfile to build.
	Dockerfile string
	// Repository is the image name without a tag, e.g. us-docker.pkg.dev/p/r/app-web.
	Repository string
	// Tag is the tag pushed alongside the digest; "latest" when empty.
	Tag      string
	Platform string
}

// MovingTag reports whether an image reference names a tag that is expected to
// move, and which tag that is. A reference with a digest never moves. Without
// one, no tag at all means "latest", and a tag with no digit in it ("latest",
// "main", "stable") reads as a channel or a branch rather than a version.
//
// A deploy from a moving tag runs whatever the tag points at when the platform
// pulls it, which behind a caching registry can be an image from days ago.
func MovingTag(ref string) (tag string, moving bool) {
	if strings.Contains(ref, "@") {
		return "", false
	}
	name := ref[strings.LastIndex(ref, "/")+1:] // a colon before that is a registry port
	_, tag, tagged := strings.Cut(name, ":")
	if !tagged || tag == "" {
		return "latest", true
	}
	return tag, !strings.ContainsAny(tag, "0123456789")
}

// Registry splits an image reference into the host it is pulled from and the
// rest. A reference that names no host, such as "nginx:1.27" or
// "acme/shop", is on Docker Hub.
func Registry(ref string) (host, rest string) {
	first, after, found := strings.Cut(ref, "/")
	if !found || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return "docker.io", ref
	}
	return strings.ToLower(first), after
}

// BuildAndPush runs `docker buildx build --push` and returns the pushed
// image as repository@sha256:digest.
func BuildAndPush(ctx context.Context, env *adapter.Env, b Build) (string, error) {
	platform := b.Platform
	if platform == "" {
		platform = DefaultPlatform
	}
	tag := b.Tag
	if tag == "" {
		tag = "latest"
	}
	tmp, err := os.MkdirTemp("", "anyship-build-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	metadata := filepath.Join(tmp, "metadata.json")

	args := []string{"buildx", "build", "--platform", platform, "--file", b.Dockerfile,
		"--tag", b.Repository + ":" + tag, "--metadata-file", metadata, "--push", b.Context}
	env.Logf("$ docker %s", strings.Join(args, " "))
	if err := env.Exec(ctx, adapter.ExecOptions{Dir: env.Dir}, "docker", args...); err != nil {
		return "", fmt.Errorf("docker buildx build failed: %w", err)
	}

	data, err := os.ReadFile(metadata)
	if err != nil {
		return "", fmt.Errorf("docker buildx didn't report the pushed image: %w", err)
	}
	var m struct {
		Digest string `json:"containerimage.digest"`
	}
	if err := json.Unmarshal(data, &m); err != nil || !strings.HasPrefix(m.Digest, "sha256:") {
		return "", fmt.Errorf("docker buildx didn't report a digest for %s", b.Repository)
	}
	return b.Repository + "@" + m.Digest, nil
}

// WriteGenerated writes a generated Dockerfile and its ignore file into dir
// as <name>.Dockerfile, and returns the Dockerfile's path. BuildKit reads the
// ignore file from next to the Dockerfile.
func WriteGenerated(dir, name string, g *dockerfile.Result) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".Dockerfile")
	if err := os.WriteFile(path, g.Dockerfile, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(path+".dockerignore", g.Ignore, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
