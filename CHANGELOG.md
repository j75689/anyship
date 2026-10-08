# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Every command a script or an agent would ask a question of has a `--json` form that matches the
  MCP tool's result, built from the same types: `init --json` (the draft, its evidence and findings,
  and the `anyship.yaml` already there under `existing`; writes nothing), `validate --json`,
  `apply --json` and `destroy --json` (the outcome with the tail of what the platform's tools
  printed), and `status --json` gains `healthy`. `init --stdout` prints the draft alone.
  `anyship targets <name>` prints a target's page from the binary, and `targets --json` says so
  under `docs` (#122).

### Fixed

- Every MCP tool now states all four behaviour hints (`readOnlyHint`, `destructiveHint`,
  `idempotentHint`, `openWorldHint`). The read-only tools left `destructiveHint` unset, which a host
  reads as the protocol's default, destructive; the tools that ask the platform (`status`, `logs`,
  `diagnose_context`) and the deploying ones left `openWorldHint` unset; and tool directories reject
  a tool with a hint missing.
- `anyship diagnose` ran the target's checks in the project, so a dry run left the generated files
  under `.anyship/<target>/` although the README says it writes nothing. The checks now run in a
  temporary directory that is removed afterwards, as the MCP tool `diagnose_context` already did, and
  the two share the code.

### Changed

- Option hints end with `anyship targets <name>` instead of a GitHub URL; over MCP they still name
  the `anyship://targets/<name>` resource (#122).
- `validate` and `detect` over MCP returned `"targets": null` for a spec without a `targets` block;
  it is `[]` now (#122).

- **Breaking:** `anyship diagnose` no longer calls Claude. It prints the redacted deployment context
  (the spec, plan findings, the target's dry-run checks, status, recent logs and generated files) for
  the agent of your choice to diagnose, the same text the MCP tool `diagnose_context` returns, and
  `--json` wraps it as `{"context": ...}`. The `--show-context`, `--yes`, `--model` and `--effort`
  flags, the proposed-change flow and the need for `ANTHROPIC_API_KEY` are gone, and anyship no
  longer depends on the Anthropic SDK (#119).

## [0.5.0] - 2026-10-08

### Added

- `kubernetes` target: deploys a spec to any cluster through `kubectl`, as a Deployment and a
  ClusterIP Service per service in a namespace you name (#97, #98). Images are built locally and
  pushed to a registry you name, for the nodes' architecture, and deployed by digest; secrets are
  Secret objects mounted as files under `/run/secrets`; `healthCheck` becomes startup and readiness
  probes, `memory` and `cpu` resource requests and limits, `replicas` the replica count. Every
  object goes in one `kubectl apply --prune`, so a service taken out of the spec is removed on the
  next `apply`, and `apply` waits for each rollout and reports the pods and warnings of one that
  fails. `status`, `logs -f` and `destroy` work. CI runs the target end to end on a kind cluster,
  and the MCP server is driven through a deploy to it.
- `kubernetes`: a public HTTP port gets an Ingress when `targets.kubernetes.ingressClass` names
  the cluster's IngressClass, with `domains` as its host rules (any host without them); preflight
  checks the class exists and lists the cluster's when it doesn't. A public TCP or UDP port makes
  the service's Service a LoadBalancer, whose address `apply` and `status` show once assigned.
  Without `ingressClass` a public port stays reachable inside the cluster only, as before (#99).
- `kubernetes`: `cron` entries become CronJobs, read in UTC: a `path` is called inside the cluster
  by curl (`targets.kubernetes.cronImage` names another image), a `command` runs in the service's
  image with its env and secrets. Runs don't overlap, an entry taken out of the spec loses its
  CronJob on the next `apply` (#100).
- `kubernetes`: `volumes` become PersistentVolumeClaims from `targets.kubernetes.storageClass` or
  the cluster's default, mounted by a Deployment that replaces its pod rather than overlapping it,
  kept until `destroy --volumes`; `static` sites are served by nginx; `worker` services run with
  no Service (#101).
- `kubernetes`: per-service settings under `targets.kubernetes.services.<name>`: `serviceAccount`,
  `maxReplicas` (a HorizontalPodAutoscaler on CPU use, needs metrics-server) and `resources`, which
  overrides the container's requests and limits slot by slot, with `none` to leave one unset, so a
  CPU limit can be dropped (#102).
- `anyship mcp` serves the spec's JSON schema (`anyship://schema`) and each target's page
  (`anyship://targets/<name>`) as resources, so an agent writing `anyship.yaml` can read a
  target's options instead of being pointed at a URL; the `targets` tool names each page (#111).
- `anyship mcp`: `status` returns `healthy`, the verdict the CLI's exit code gives; `logs` takes
  `timestamps`; `apply` names the files it wrote under `.anyship/<target>/` (#111).
- `anyship mcp`: `detect` reports an `anyship.yaml` already in the directory (`existing`, with its
  validation, services and targets) and the instructions say to validate and plan that one;
  `validate` lists the spec's targets (#115).
- `kubernetes`: a new pod has to stay ready for 5 seconds before the rollout counts it, so a
  process that dies at startup fails the apply instead of passing as deployed (#115).
- The spec refuses an env value that reads as a reference to a service's URL but isn't written as
  `${services.<name>.url}` (`${service.api.url}`, `services.api.url`, `${services.api.port}`, …),
  since the container would get that text as it is (#104).

## [0.4.1] - 2026-10-07

### Fixed

- `gcp`: a cron job called the service at its `status.url` and made the identity token out to
  that, while `${services.<name>.url}` expands to the service's other URL, so an app that checks
  the token's audience against the URL from its env rejected every tick (#91). The job now uses
  `${services.<name>.url}` for both.

## [0.4.0] - 2026-10-07

### ⚠ BREAKING CHANGES

- **`gcp`: every deploy sets the instance size from the spec.** A service that names no `memory`
  or `cpu` is deployed with Cloud Run's defaults, 512MB and 1 CPU, so a size set by hand with
  `gcloud run services update` is replaced on the next `apply`. Before upgrading, read the current
  values (`gcloud run services describe <service> --format 'value(spec.template.spec.containers[0].resources.limits)'`)
  and put them in the spec as `memory` and `cpu`.
- **`gcp`: the same goes for the other settings anyship knows.** Every deploy passes the minimum and
  maximum instance count, concurrency, request timeout and service account, as the spec's value or
  as Cloud Run's default. `targets.gcp.timeout` no longer keeps the service's timeout when it is
  left out: the service goes back to 5 minutes. Lowering `replicas` to 1 now takes the minimum
  instance count off the service, which it didn't before. Put values you set by hand under
  `targets.gcp.services.<name>` before upgrading.

### Added

- `${services.<name>.url}` in a service's `env` is the URL of another service of the spec, on
  whichever target deploys it: on `gcp` the service's Cloud Run URL, known before the first
  deploy; on `vps` its name and port on the compose network. `aws` and `cloudflare` refuse it with
  a reason, since neither URL is known before a deploy. A reference to a service the spec doesn't
  have is a validation error.
- `gcp`: the accounts of the spec's other services get `roles/run.invoker` on each service with an
  `internal` port, and `targets.gcp.services.<name>.ingress: all` opens such a service to requests
  from anywhere with the identity token as the only guard, so the services of a spec can call each
  other without a VPC. Without it an internal port keeps internal ingress, as before, and
  `targets.gcp.network` and `subnet` name an existing VPC through which the services that refer to
  it send their traffic, so they reach it there; preflight checks the subnet.
- Services take `memory` (`512MB`, `4GB`) and `cpu` (`0.5`, `1`, `2`): what one instance needs,
  on any target. `gcp` sizes the Cloud Run instance and refuses a pair Cloud Run doesn't offer,
  `aws` sizes the task (the service's values win over `targets.aws.cpu` and `memory`), `vps` limits
  the container, and `cloudflare` warns that Workers can't be sized.
- `gcp`: `targets.gcp.services.<name>` holds the Cloud Run settings of one service: `maxInstances`,
  `concurrency`, `timeout`, `serviceAccount` and `executionEnvironment`. With `concurrency: 1` a
  service can run on less than one `cpu`.
- A `cron` entry can call the service over HTTP instead of running a command: `path` (and `method`,
  `POST` by default). `gcp` runs these on Cloud Scheduler, with an identity token for the account
  the service runs as; jobs are found by name, an entry taken out of the spec loses its job on the
  next `apply`, and `destroy` removes them. `cloudflare` warns that cron triggers call
  `scheduled()`, not a path.

### Changed

- `gcp`: each service account is let into the secrets of the services that run as it, not into
  every secret of the spec, and every `apply` takes the right to read a secret away from service
  accounts the spec doesn't give it to (`apply --dry-run` lists them first). Accounts outside the
  spec that should keep reading go in `targets.gcp.secretReaders`. Only secrets anyship created
  for the spec, only that role, and only service accounts are touched.

### Fixed

- `gcp`: `healthCheck` was dropped without a word. `healthCheck.path` is now the Cloud Run startup
  probe, so a revision that can't answer it never gets traffic, and `plan` says that
  `healthCheck.command` isn't applied. A spec that already names a path starts being checked on its
  next deploy: the path has to answer with a 2xx or 3xx status, without credentials, within 4
  minutes, or the deploy fails and the previous revision keeps serving. A spec without a path
  removes the service's startup probe, so one set by hand needs `healthCheck.path` to stay.
- `gcp`: `plan` refuses an image on a registry Cloud Run can't pull from, such as `quay.io`
  (`GCP_UNPULLABLE_IMAGE`), and says how to reach it through Artifact Registry. It used to pass and
  fail in `gcloud run deploy`. A `ghcr.io` image gets a warning instead (`GCP_GHCR_PUBLIC_ONLY`):
  Cloud Run pulls public ones through a cache of its own, and private ones not at all.
- `gcp`: `plan` warns that a service with an `internal` port can't be called by the spec's other
  services as deployed (`GCP_INTERNAL_CALLERS`), and lists what a caller needs: a route through a
  VPC network, `roles/run.invoker`, and an identity token. It used to say nothing.

## [0.3.1] - 2026-10-06

### Added

- `gcp`: `targets.gcp.timeout` sets Cloud Run's request timeout (for example `10m`, up to `1h`)
  for every service; without it a deploy keeps the current one.
- `plan` and `apply` take `--image <service>=<image>` (and the MCP tools an `images` argument) to
  deploy a different image than the spec names, for that run only, so a pipeline can deploy the
  digest it just built without editing `anyship.yaml`.
- `gcp`, `aws`: `plan` warns when a service is deployed by a tag that moves (`GCP_MUTABLE_TAG`,
  `AWS_MUTABLE_TAG`). Such a tag can resolve to a stale image behind a caching registry while the
  deploy reports success.

### Changed

- Built with Go 1.27. Building from source needs Go 1.27 or newer; an older `go` downloads it on its
  own.

### Fixed

- `vps`: an image named by a tag that moves (`:latest`, `:main`, no tag) is pulled on every `apply`.
  Compose kept the image the host already had, so pushing the tag again and re-deploying changed
  nothing while reporting success. `plan` notes such services (`VPS_MUTABLE_TAG`).
- `aws`: `destroy` no longer answers "Nothing to remove" while a service from an earlier destroy is
  still draining. It names the services still being deleted and says their load balancers bill
  until that finishes.
- `vps`, `gcp`, `aws`: the hint for a missing or wrong target block showed JSON to paste into what is
  now a YAML file. Hints and messages name the block's path (`spec.targets.<name>`), show the fields
  as they are written in `anyship.yaml`, and link the target's page.
- `vps`: a failed `status` says why. It reported only ssh's exit status; now the error carries what
  ssh or Docker said, and how to check the connection when ssh could not connect.
- A value of the wrong shape in `anyship.yaml` is named by its path and by what belongs there
  (`spec.secrets: must be a mapping (key: value), not a list`) instead of a Go decoding error, a
  number or boolean where text belongs comes with the advice to quote it, and every such problem is
  reported at once. Unknown fields now carry their path and the field that was probably meant.
  Target blocks get the same treatment.
- `plan` over MCP and `plan --json` return each generated file with its contents (`files[].path`,
  `files[].contents`). They listed paths that do not exist until a deploy writes them, next to a
  finding telling the reader to review them. Over MCP this changes the shape of `files`, which was
  a list of paths.
- MCP: `diagnose_context`, which is annotated read-only, no longer writes `.anyship/<target>/` into
  the project; it runs the target's checks in a temporary directory.
- A missing spec is reported by the full path that was tried, not as a bare `anyship.yaml`. Over MCP
  the server's instructions say which directory it runs in, `detect` returns the directory it
  inspected, and the error points at the `detect` tool instead of a shell command.
- `init` and `detect` at the root of a repository with several apps name the subdirectories that
  look like projects, instead of suggesting a Dockerfile.
- MCP: messages name tools rather than shell commands ("has it been deployed with the apply tool?"
  where the CLI says `anyship apply -t vps`), since an agent may have no shell. Read-only tools are
  now listed as idempotent; the hint was sent as false.

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

[Unreleased]: https://github.com/j75689/anyship/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/j75689/anyship/compare/v0.4.1...v0.5.0
[0.4.1]: https://github.com/j75689/anyship/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/j75689/anyship/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/j75689/anyship/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/j75689/anyship/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/j75689/anyship/releases/tag/v0.2.0
