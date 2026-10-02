# anyship architecture

anyship is one Go binary that turns `anyship.yaml` into a running app on a platform you pick, then
helps you operate it. This page describes the target architecture; parts not built yet are marked
**planned**.

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

| Package | Owns | Talks to | Status |
|---|---|---|---|
| `spec` | `anyship.yaml` types, validation, JSON Schema | nothing | built |
| `detect` | drafting a spec from JS, Go, Python, Rust, Dockerfile or static projects | project files | built |
| `dockerfile` | Dockerfiles for services that have none | project files | built |
| `image` | building an image with `docker buildx` and pushing it to a registry, by digest | Docker, the registry | built |
| `diagnose` | context collection, redaction, JSON Patch, the retry loop | Claude API | built |
| `adapter` | the contract (`Plan`, `Apply`, optional `Logs`, `Status`, `Destroy`) and registry | nothing | built |
| `adapters/cloudflare` | `wrangler.jsonc`, Workers compatibility checks | `npx wrangler` | built |
| `adapters/vps` | compose rendering, preflight, upload, status, logs, destroy | `ssh` to a Linux host | built |
| `adapters/gcp` | Cloud Run services, Secret Manager, Artifact Registry | `gcloud`, `docker` | built |
| `adapters/aws` | a managed container service, Secrets Manager, ECR | `aws`, `docker` | **planned** |
| `internal/cli` | the commands and the MCP server | everything above | built |

## Targets

Each adapter talks to its provider through the provider's own CLI, so your existing login is used
and anyship handles no cloud credentials.

| anyship.yaml asks for | cloudflare | vps | gcp | aws (planned) |
|---|---|---|---|---|
| HTTP `server` | Worker | container | Cloud Run service | container service |
| image or Dockerfile | refused | built on the host | built locally, pushed to Artifact Registry | built locally, pushed to ECR |
| `static` site | Worker assets | nginx container | refused (use cloudflare) | refused (use cloudflare) |
| volumes, TCP/UDP ports | refused | Docker volumes, published ports | refused (use vps) | refused (use vps) |
| `replicas` | automatic | Compose replicas | min instances | desired count |
| `secrets` | `wrangler secret` | files on the host | Secret Manager, by name | Secrets Manager, by name |
| databases | D1, KV, R2, Hyperdrive by id | external only | external only | external only |
| status, logs, destroy | logs, destroy | all three | all three | all three |

## Flows

### `apply -t vps`

![apply on the vps target: plan, then three ssh calls for preflight, upload and deploy](architecture/apply-vps.svg)

Nothing on the host changes until preflight passes. CI runs this path against its own runner on every
pull request (`vps-e2e`).

### `apply` on a cloud target (gcp; aws planned)

1. **plan**: render the provider's service settings and the Dockerfile, as for vps.
2. **preflight**: check the CLI login, project or account, region, required APIs and registry access.
3. **image**: `docker buildx build --platform linux/amd64 --push`, then deploy by digest so every
   apply rolls out exactly the image it built.
4. **deploy**: create or update the service by its name (`<spec name>-<service>`), labeled
   `anyship-project=<spec name>` so later commands can find it without state.

### `diagnose`

![diagnose: collect, redact, ask Claude, validate the patch, confirm, write](architecture/diagnose.svg)

## Checks and releases

- **CI on every pull request**: `check` (gofmt, vet, race tests, golangci-lint), `images` (seven sample
  apps built and served), `vps-e2e` (the runner deploys to itself over ssh), `release-snapshot`
  (every release archive built and one installed with `install.sh`).
- **Releases**: pushing a `vX.Y.Z` tag runs the tests, then GoReleaser publishes archives for Linux,
  macOS and Windows on amd64 and arm64 with `checksums.txt`; `install.sh` verifies the checksum
  before installing.
