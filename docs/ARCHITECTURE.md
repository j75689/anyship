# anyship architecture

anyship is one Go binary that turns `anyship.yaml` into a running app on a platform you pick, then
helps you operate it.

![System map: callers, the anyship binary, its adapters and the providers where all state lives](architecture/system.svg)

## Rules every part follows

- **No state.** anyship never records what it deployed. `status`, `logs`, `destroy` and redeploy
  checks ask the provider. `.anyship/<target>/` is generated output for review: written, never read
  back. Deployments are addressed from `anyship.yaml` alone.
- **Applications, not infrastructure.** Servers, networks, DNS, accounts and managed databases are
  provided by you or by Terraform/OpenTofu. anyship binds to what exists; anything it creates (such as
  a secret) is found by name on the provider, so repeating a command is safe.
- **Plan before apply.** `Plan` has no side effects. `Apply` refuses a plan with errors and runs the
  target's preflight checks before changing anything.
- **Refuse, don't drop.** A target that can't provide something the spec asks for returns an error
  finding with a stable code and a hint, never a silent downgrade.
- **AI off the deploy path.** The model only proposes spec changes; they are validated, shown, and
  written only on confirmation.

## Packages

| Package | Owns | Talks to |
|---|---|---|
| `spec` | `anyship.yaml` types, validation, JSON Schema | nothing |
| `detect` | drafting a spec from JS, Go, Python, Rust, Dockerfile or static projects | project files |
| `dockerfile` | Dockerfiles for services that have none | project files |
| `image` | building an image with `docker buildx` and pushing it to a registry, by digest | Docker, the registry |
| `diagnose` | context collection, redaction, JSON Patch, the retry loop | Claude API |
| `adapter` | the contract (`Plan`, `Apply`, optional `Logs`, `Status`, `Destroy`) and registry | nothing |
| `adapters/cloudflare` | `wrangler.jsonc`, Workers compatibility checks | `npx wrangler` |
| `adapters/vps` | compose rendering, preflight, upload, status, logs, destroy | `ssh` to a Linux host |
| `adapters/gcp` | Cloud Run services, Secret Manager, Artifact Registry | `gcloud`, `docker` |
| `adapters/aws` | ECS Express Mode services, Secrets Manager, ECR | `aws`, `docker` |
| `internal/cli` | the commands and the MCP server | everything above |

## Targets

Each adapter talks to its provider through the provider's own CLI, so your existing login is used
and anyship handles no cloud credentials. Each target has a page under [targets/](targets/) with its options
and limits.

| anyship.yaml asks for | cloudflare | vps | gcp | aws |
|---|---|---|---|---|
| HTTP `server` | Worker | container | Cloud Run service | ECS Express Mode service |
| image or Dockerfile | refused | built on the host | built locally, pushed to Artifact Registry | built locally, pushed to ECR |
| `static` site | Worker assets | nginx container | refused (use cloudflare) | refused (use cloudflare) |
| volumes, TCP/UDP ports | refused | Docker volumes, published ports | refused (use vps) | refused (use vps) |
| `domains` | custom-domain routes | refused (no HTTP router yet) | refused (map them yourself) | refused (CNAME the `on.aws` URL) |
| `replicas` | automatic | Compose replicas | min instances | min tasks |
| `secrets` | `wrangler secret` | files on the host | Secret Manager, by name | Secrets Manager, by name |
| databases | D1, KV, R2, Hyperdrive by id | external only | external only | external only |
| status, logs, destroy | logs, destroy | all three | all three | all three |

`services.<name>.domains` is the platform-neutral place to name the hosts a service answers on. Per
"applications, not infrastructure", anyship attaches them to the platform's HTTP router where it can
and issues no certificates and creates no DNS records. A target's own block may override the field:
`targets.cloudflare.domains` replaces it rather than adding to it, so a Cloudflare deploy can use
different hosts than the rest of the spec, and `plan` notes the substitution (`CF_DOMAIN_OVERRIDE`).

## Flows

### `apply -t vps`

![apply on the vps target: plan, then three ssh calls for preflight, upload and deploy](architecture/apply-vps.svg)

Nothing on the host changes until preflight passes. CI runs this path against its own runner on every
pull request (`vps-e2e`).

### `apply` on a cloud target (gcp, aws)

1. **plan**: render the provider's service settings and the Dockerfile, as for vps.
2. **preflight**: check the CLI login, project or account, region, required APIs and registry access.
3. **image**: `docker buildx build --platform linux/amd64 --push`, then deploy by digest so every
   apply rolls out exactly the image it built.
4. **deploy**: create or update the service by its name (`<spec name>-<service>`), labeled or tagged
   `anyship-project=<spec name>`. Later commands find it by that name or label, without state.

### `diagnose`

![diagnose: collect, redact, ask Claude, validate the patch, confirm, write](architecture/diagnose.svg)

## Checks and releases

- **CI on every pull request**: `lint` (gofmt, vet, `go mod tidy`, golangci-lint), `test` and
  `test-windows` (unit tests; with the race detector on Linux), `build` (the binary, and every
  package for each release platform), `images` (seven sample apps built and served), `vps-e2e` (the
  runner deploys to itself over ssh), `release-snapshot` (every release archive built and one
  installed with `install.sh`). `make check` runs the first three locally.
- **Releases**: pushing a `vX.Y.Z` tag runs the tests, then GoReleaser publishes archives for Linux,
  macOS and Windows on amd64 and arm64 with `checksums.txt`; `install.sh` verifies the checksum
  before installing.
