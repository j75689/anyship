package spec

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// TestMigrateGolden converts every testdata/migrate/*.json and compares the
// result with the .yaml next to it. Run with -update to rewrite them.
func TestMigrateGolden(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "migrate", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 3 {
		t.Fatalf("got %d fixtures, want at least 3", len(files))
	}
	for _, file := range files {
		t.Run(strings.TrimSuffix(filepath.Base(file), ".json"), func(t *testing.T) {
			legacy, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Migrate(legacy)
			if err != nil {
				t.Fatalf("Migrate: %v", err)
			}

			golden := strings.TrimSuffix(file, ".json") + ".yaml"
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run `go test ./spec -update` to create it)", err)
			}
			if string(got) != string(want) {
				t.Errorf("%s does not match:\ngot:\n%s\nwant:\n%s", filepath.Base(golden), got, want)
			}

			// The whole point of migrating is to end up with a spec anyship
			// accepts, so every fixture has to parse and validate.
			s, err := Parse(got)
			if err != nil {
				t.Fatalf("the converted spec does not parse: %v", err)
			}
			if problems := s.Validate(); len(problems) > 0 {
				t.Errorf("the converted spec does not validate: %v", problems)
			}
		})
	}
}

func TestMigrateKeepsEverythingTheSpecStillHas(t *testing.T) {
	got, err := Migrate([]byte(`{
		"version": 1,
		"name": "eth-mainnet",
		"services": {
			"reth": {
				"kind": "server",
				"image": "ghcr.io/paradigmxyz/reth:latest",
				"ports": [{"name": "p2p", "port": 30303, "protocol": "tcp+udp"}],
				"volumes": [{"name": "data", "mountPath": "/data", "size": "2TB", "class": "nvme"}],
				"secrets": ["JWT_SECRET"]
			}
		},
		"secrets": {"JWT_SECRET": {"generate": "hex32"}},
		"targets": {"vps": {"host": "deploy@203.0.113.10"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "eth-mainnet" {
		t.Errorf("metadata.name = %q, want eth-mainnet", s.Name)
	}
	svc := s.Services["reth"]
	if svc.Image != "ghcr.io/paradigmxyz/reth:latest" || svc.Ports[0].Protocol != ProtocolTCPUDP {
		t.Errorf("services.reth did not survive: %+v", svc)
	}
	if svc.Volumes[0].Size != "2TB" || svc.Volumes[0].Class != "nvme" {
		t.Errorf("volumes did not survive: %+v", svc.Volumes)
	}
	if string(s.Targets["vps"]) != `{"host":"deploy@203.0.113.10"}` {
		t.Errorf("targets.vps = %s", s.Targets["vps"])
	}
}

func TestMigrateRejectsWhatItCannotConvert(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"not JSON": {
			in:   `services:\n  web: {}`,
			want: "not a valid anyship.json",
		},
		"trailing data": {
			in:   `{"version": 1, "name": "a", "services": {}} {}`,
			want: "unexpected data after the top-level object",
		},
		"already a manifest": {
			in:   `{"apiVersion": "anyship/v1alpha1", "kind": "App", "metadata": {}, "spec": {}}`,
			want: "already an anyship/v1alpha1 manifest",
		},
		"no version": {
			in:   `{"name": "a", "services": {}}`,
			want: "version: is required",
		},
		"version 2": {
			in:   `{"version": 2, "name": "a", "services": {}}`,
			want: "version: must be 1",
		},
		"unknown top-level field": {
			in:   `{"version": 1, "name": "a", "services": {}, "regions": ["eu"]}`,
			want: `regions: unknown field; known fields are $schema, version, name, services, resources, secrets, targets`,
		},
		"misspelled top-level field": {
			in:   `{"version": 1, "name": "a", "service": {}}`,
			want: `service: unknown field; did you mean "services"?`,
		},
		"unknown service field": {
			in:   `{"version": 1, "name": "a", "services": {"web": {"kind": "server", "regions": ["eu"]}}}`,
			want: "spec.services.web.regions: unknown field",
		},
		"wrong case in a service field": {
			in:   `{"version": 1, "name": "a", "services": {"web": {"kind": "server", "healthcheck": {}}}}`,
			want: `spec.services.web.healthcheck: unknown field; did you mean "healthCheck"?`,
		},
		"unknown field inside a list": {
			in:   `{"version": 1, "name": "a", "services": {"web": {"kind": "server", "ports": [{"port": 80, "public": true}]}}}`,
			want: "spec.services.web.ports.0.public: unknown field",
		},
		"unknown field inside an object": {
			in:   `{"version": 1, "name": "a", "services": {"web": {"kind": "server", "build": {"dir": "dist"}}}}`,
			want: "spec.services.web.build.dir: unknown field",
		},
		"unknown resource field": {
			in:   `{"version": 1, "name": "a", "services": {}, "resources": {"db": {"type": "postgres", "version": "16"}}}`,
			want: "spec.resources.db.version: unknown field",
		},
		"value of the wrong type": {
			in:   `{"version": 1, "name": "a", "services": {"web": {"kind": "server", "replicas": "two"}}}`,
			want: "not a valid anyship.json",
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := Migrate([]byte(tc.in))
			if err == nil {
				t.Fatalf("Migrate accepted it and wrote:\n%s", out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A target block belongs to its adapter, so migrate passes it through whole
// instead of second-guessing fields it does not know.
func TestMigrateDoesNotCheckTargetBlocks(t *testing.T) {
	got, err := Migrate([]byte(`{"version": 1, "name": "a", "services": {"web": {"kind": "server", "start": "x"}},
		"targets": {"fly": {"anything": {"at": ["all"]}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "anything:") {
		t.Errorf("the target block was dropped:\n%s", got)
	}
}

// A spec that was already broken migrates into a broken spec: the conversion
// succeeds and parsing reports the problem, so the user can fix it in the new
// format instead of the old one.
func TestMigrateConvertsAnInvalidSpec(t *testing.T) {
	got, err := Migrate([]byte(`{"version": 1, "name": "a",
		"services": {"web": {"kind": "server", "start": "x", "uses": ["db"]}}}`))
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := Parse(got); err == nil || !strings.Contains(err.Error(), `unknown resource "db"`) {
		t.Errorf("Parse error = %v, want it to mention the undeclared resource", err)
	}
}

// The migration guide shows a v0.2 spec and the v0.3 spec it becomes. Migrate
// has to agree with every one of those pairs, or the guide documents a
// conversion the command does not perform.
func TestMigrationGuidePairsMatchMigrate(t *testing.T) {
	var pairs int
	var pending []byte // the most recent "before" spec, waiting for its "after"
	for _, block := range fencedBlocks(t, filepath.Join("..", MigrationGuide)) {
		switch {
		case block.lang == "json" && strings.Contains(block.body, `"version": 1`):
			pending = []byte(block.body)
		case block.lang == "yaml" && strings.Contains(block.body, "apiVersion:") && pending != nil:
			got, err := Migrate(pending)
			if err != nil {
				t.Fatalf("Migrate rejected a guide example: %v\n%s", err, pending)
			}
			migrated, err := Parse(got)
			if err != nil {
				t.Fatalf("Migrate produced an invalid spec: %v\n%s", err, got)
			}
			if documented := mustParse(t, []byte(block.body)); !reflect.DeepEqual(migrated, documented) {
				t.Errorf("migrate does not produce the documented spec:\ngot:\n%s", got)
			}
			pairs, pending = pairs+1, nil
		}
	}
	if pairs < 2 {
		t.Errorf("got %d before/after pairs, want at least 2", pairs)
	}
}

func TestNearest(t *testing.T) {
	known := []string{"services", "resources", "secrets", "targets"}
	for key, want := range map[string]string{
		"service":   "services",
		"Services":  "services",
		"secret":    "secrets",
		"resourses": "resources",
		"regions":   "",
		"x":         "",
	} {
		if got := nearest(key, known); got != want {
			t.Errorf("nearest(%q) = %q, want %q", key, got, want)
		}
	}
}
