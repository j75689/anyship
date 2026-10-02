package dockerfile_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/j75689/anyship/adapter"
	"github.com/j75689/anyship/detect"
	"github.com/j75689/anyship/dockerfile"
	"github.com/j75689/anyship/spec"
)

// TestGeneratedImagesServeHTTP runs the whole path for each app under
// testdata/apps: detect the project, generate a Dockerfile, build the image,
// run it and expect HTTP 200. It needs a Docker daemon and network access,
// so it only runs with ANYSHIP_DOCKER_TESTS=1 (CI sets it).
func TestGeneratedImagesServeHTTP(t *testing.T) {
	if os.Getenv("ANYSHIP_DOCKER_TESTS") == "" {
		t.Skip("set ANYSHIP_DOCKER_TESTS=1 to build and run the generated images")
	}
	apps, err := os.ReadDir(filepath.Join("testdata", "apps"))
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range apps {
		t.Run(app.Name(), func(t *testing.T) {
			src := filepath.Join("testdata", "apps", app.Name())
			svc := detectService(t, src)

			generated, err := dockerfile.Generate(svc, src)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("generated Dockerfile:\n%s", generated.Dockerfile)

			buildDir := t.TempDir()
			if err := os.CopyFS(buildDir, os.DirFS(src)); err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string][]byte{dockerfile.Filename: generated.Dockerfile, dockerfile.IgnoreFilename: generated.Ignore} {
				if err := os.WriteFile(filepath.Join(buildDir, name), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			image := "anyship-test-" + app.Name()
			docker(t, "build", "-q", "-f", filepath.Join(buildDir, dockerfile.Filename), "-t", image, buildDir)
			t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", image).Run() })

			port := dockerfile.StaticPort
			if svc.Kind != spec.KindStatic {
				port = svc.Ports[0].Port
			}
			container := docker(t, "run", "-d", "-p", fmt.Sprintf("127.0.0.1::%d", port), image)
			t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })
			address := docker(t, "port", container, fmt.Sprintf("%d/tcp", port))

			if err := waitForOK("http://"+strings.Split(address, "\n")[0]+"/", 90*time.Second); err != nil {
				logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
				t.Fatalf("%v\ncontainer logs:\n%s", err, logs)
			}
		})
	}
}

func detectService(t *testing.T, dir string) *spec.Service {
	t.Helper()
	d, err := detect.Project(dir)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.HasErrors(d.Findings) {
		t.Fatalf("detection failed: %+v", d.Findings)
	}
	data, err := spec.Marshal(d.Spec)
	if err != nil {
		t.Fatal(err)
	}
	s, err := spec.Parse(data) // normalizes defaults, as `anyship apply` would see them
	if err != nil {
		t.Fatal(err)
	}
	return s.Services[detect.ServiceName]
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func waitForOK(url string, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = resp.Status
		} else {
			last = err.Error()
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("GET %s never returned 200 (last: %s)", url, last)
}
