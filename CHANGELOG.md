# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.3.0] - unreleased

### ⚠ BREAKING CHANGES

- **The spec is a YAML manifest: `anyship.json` is no longer read.** Move the file to `anyship.yaml`,
  put `name` under `metadata`, the rest under `spec`, and add `apiVersion: anyship/v1alpha1` and
  `kind: App`. Everything under `services`, `resources`, `secrets` and `targets` is unchanged. See
  [docs/MIGRATION.md](docs/MIGRATION.md) for the field-by-field mapping and complete before/after
  examples. Loading a project that still has `anyship.json` fails with a pointer to that guide.
- `diagnose` JSON Pointers now start at the manifest root (`/spec/services/...`), and validation
  problems use the matching paths (`spec.services.web...`).

### Added

- `gcp` target: a Google Cloud Run adapter. Each service becomes the Cloud Run service
  `<spec>-<service>`, labeled `anyship-project=<spec>` so `status`, `logs` and `destroy` find it
  without state. Images are pushed to your Artifact Registry repository and deployed by digest;
  secrets live in Secret Manager. `apply` runs preflight checks first and `--dry-run` stops after
  them. Static sites, workers, volumes, cron and TCP/UDP ports are refused with a reason.
- `aws` target: an Amazon ECS Express Mode adapter, which provisions the load balancer, HTTPS URL and
  autoscaling for each service. Images go to your ECR repository and are deployed by digest; secrets
  live in Secrets Manager. `logs` tails CloudWatch Logs, including `-f`.
- `anyship init` and the MCP `detect` tool write YAML with a `yaml-language-server` schema comment, so
  editors validate and autocomplete the spec.
- `vps` honours `healthCheck`: `path` becomes a Compose `healthcheck` that requests it with `wget` or
  `curl`, and `apply` waits for every checked container to report healthy. A service that never does
  makes `apply` fail with the reason and that container's last log lines, instead of reporting a
  success. An image with neither `wget` nor `curl` is reported as unchecked rather than skipped
  quietly.

### Changed

- Images are built and pushed with `docker buildx build --push` for `linux/amd64` and deployed by
  `repository@sha256:digest`, so a deploy runs exactly the image it built. Builds take a tag
  (`latest` by default) for registries that hold several services in one repository.
- Every cloud adapter deploys dependencies first, in one shared order (`Spec.DeployOrder`).
- `diagnose` patches the YAML tree in place, so comments and key order survive its fixes.

## [0.2.0] - 2026-10-02

The first tagged release. It covers everything built up to that point.

### Added

- `anyship.json` spec: a platform-neutral description of services, ports, volumes, resources, secrets
  and per-target overrides, with strict validation and a generated JSON Schema.
- Commands: `init`, `validate`, `plan`, `apply`, `status`, `logs`, `destroy`, `targets`, `schema`,
  `diagnose`, and `--json` output.
- `cloudflare` target: Workers (edge handlers) and static assets; D1, KV, R2 and Hyperdrive bindings;
  cron triggers; custom domains.
- `vps` target: any Linux server with Docker, over SSH. Images or Dockerfiles, multi-service,
  volumes, TCP/UDP ports, secrets, host preflight checks, and `destroy --dry-run`.
- Project detection for JavaScript, Go, Python and Rust, and generated Dockerfiles for services that
  have none.
- MCP server (`anyship mcp`), so AI agents can read a project, plan and — only with `--allow-deploy` —
  deploy it.
- `diagnose`: collects redacted deployment context and asks Claude for a validated spec fix
  (bring your own key).
- Release binaries built with GoReleaser, and `install.sh`, which verifies checksums.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): the system map and the main flows.

[0.3.0]: https://github.com/j75689/anyship/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/j75689/anyship/releases/tag/v0.2.0
