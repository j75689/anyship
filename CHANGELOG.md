# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `gcp`: `targets.gcp.timeout` sets Cloud Run's request timeout (for example `10m`, up to `1h`)
  for every service; without it a deploy keeps the current one.

### Fixed

- `aws`: `destroy` no longer answers "Nothing to remove" while a service from an earlier destroy is
  still draining. It names the services still being deleted and says their load balancers bill
  until that finishes.
- `vps`, `gcp`, `aws`: the hint for a missing or wrong target block showed JSON to paste into what is
  now a YAML file. Hints and messages name the block's path (`spec.targets.<name>`), show the fields
  as they are written in `anyship.yaml`, and link the target's page.
- `vps`: a failed `status` says why. It reported only ssh's exit status; now the error carries what
  ssh or Docker said, and how to check the connection when ssh could not connect.

## [0.3.0] - 2026-10-06

### ⚠ BREAKING CHANGES

- **The spec is a YAML manifest: `anyship.json` is no longer read.** Rename the file to `anyship.yaml`,
  drop `$schema` and `version`, put `name` under `metadata`, the rest under `spec`, and add
  `apiVersion: anyship/v1alpha1` and `kind: App`. Everything under `services`, `resources`, `secrets`
  and `targets` is unchanged, and JSON syntax is still valid inside the YAML file. Loading a project
  that still has `anyship.json` fails with a pointer to this note.
- `diagnose` JSON Pointers now start at the manifest root (`/spec/services/...`), and validation
  problems use the matching paths (`spec.services.web...`).

### Added

- `gcp` target: a Google Cloud Run adapter. Each service becomes the Cloud Run service
  `<spec>-<service>`, labeled `anyship-project=<spec>` so `status`, `logs` and `destroy` find it
  without state. Images are pushed to your Artifact Registry repository and deployed by digest;
  secrets live in Secret Manager, and each apply lets the account the services run as read the ones
  the spec uses. `targets.gcp.configuration` picks the gcloud configuration and `serviceAccount` that
  account. `apply` runs preflight checks first and `--dry-run` stops after them. Static sites,
  workers, volumes, cron and TCP/UDP ports are refused with a reason.
- `aws` target: an Amazon ECS Express Mode adapter, which provisions the load balancer, HTTPS URL and
  autoscaling for each service. Images go to your ECR repository and are deployed by digest; secrets
  live in Secrets Manager. `logs` tails CloudWatch Logs, including `-f`.
- `anyship init` and the MCP `detect` tool write YAML with a `yaml-language-server` schema comment, so
  editors validate and autocomplete the spec.
- `services.<name>.domains`: custom domains live in the spec, in one place for every target. The
  `cloudflare` target attaches them; `vps`, `gcp` and `aws` refuse them with a
  `*_DOMAIN_UNSUPPORTED` finding and a hint. `targets.cloudflare.domains` still works and replaces
  the service's list for that target.
- `anyship diagnose` and the MCP `diagnose_context` tool: collect redacted deployment context and ask
  Claude for a root cause and a validated spec fix (bring your own key). Experimental: tested
  against a stubbed API, not yet run against the live one.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), the system map and the main flows, and a page for
  each target under [docs/targets/](docs/targets/).
- [SECURITY.md](SECURITY.md): how to report a vulnerability privately.

### Changed

- Images are built and pushed with `docker buildx build --push` for `linux/amd64` and deployed by
  `repository@sha256:digest`, so a deploy runs exactly the image it built. Builds take a tag
  (`latest` by default) for registries that hold several services in one repository.
- Every cloud adapter deploys dependencies first, in one shared order (`Spec.DeployOrder`).
- `diagnose` patches the YAML tree in place, so comments and key order survive its fixes.
- Release archives are built with `go build` instead of GoReleaser. Their names and `checksums.txt`
  are unchanged, each one now includes `NOTICE`, and release notes are this file's section for the
  version.

### Fixed

- Windows: commands run directly instead of through `cmd /C`, which mangled any argument holding a
  quote, `&` or `^`, such as the script the `vps` target hands to ssh.
- Spec paths written with Windows separators are normalized to slashes, so a spec means the same
  thing on every machine, and a `build.output` that names a Windows drive is refused.
- `cloudflare`: Workers are addressed from the spec, never from a leftover generated config.

## [0.2.0] - 2026-10-02

The first tagged release. It covers everything built up to that point.

### Added

- `anyship.json` spec: a platform-neutral description of services, ports, volumes, resources, secrets
  and per-target overrides, with strict validation and a generated JSON Schema.
- Commands: `init`, `validate`, `plan`, `apply`, `status`, `logs`, `destroy`, `targets`, `schema`,
  and `--json` output.
- `cloudflare` target: Workers (edge handlers) and static assets; D1, KV, R2 and Hyperdrive bindings;
  cron triggers; custom domains.
- `vps` target: any Linux server with Docker, over SSH. Images or Dockerfiles, multi-service,
  volumes, TCP/UDP ports, secrets, host preflight checks, and `destroy --dry-run`.
- Project detection for JavaScript, Go, Python and Rust, and generated Dockerfiles for services that
  have none.
- MCP server (`anyship mcp`), so AI agents can read a project, plan and — only with `--allow-deploy` —
  deploy it.
- Release binaries built with GoReleaser, and `install.sh`, which verifies checksums.

[Unreleased]: https://github.com/j75689/anyship/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/j75689/anyship/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/j75689/anyship/releases/tag/v0.2.0
