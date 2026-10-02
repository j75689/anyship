# anyship

**Describe your app once. Deploy it anywhere.**

anyship reads your project, drafts a platform-neutral deploy spec (`anyship.yaml`), and turns that one
spec into a deployment on whichever platform you pick. Each platform adapter either satisfies every
need in the spec or tells you exactly which need it can't meet and why. It never quietly drops one.

> **Status: early (v0.2).** The spec, rule-based detection, the CLI, a Cloudflare Workers adapter, a
> VPS (Docker over SSH) adapter and a Google Cloud Run adapter work today. More targets and the AI assistant layer are on the [roadmap](#roadmap).

```console
$ anyship init
Detected:
  • package manager: npm
  • dependency "hono" → hono (server)
  • edge entry module: src/index.ts
✔ wrote anyship.yaml

$ anyship plan --target cloudflare
Plan for cloudflare
Actions:
  ↑ worker hono-worker  wrangler deploy --config .anyship/cloudflare/wrangler.jsonc
Plan is ready.

$ anyship apply --target cloudflare
```

## Why

Every platform has its own config format (`wrangler.jsonc`, `fly.toml`, `vercel.json`, `railway.toml`, ...)
and its own limits. anyship splits deployment into three layers:

1. **Understand.** Detect the stack and draft `anyship.yaml`. Rule-based and deterministic; the
   optional AI layer only *proposes* edits to the spec for you to review.
2. **Describe.** `anyship.yaml` is the single source of truth: services, ports, disks, databases, secrets.
   You commit it, and every deploy reads it, so deploys are reproducible.
3. **Deliver.** Adapters translate the spec into platform config deterministically, with a
   Terraform-style `plan` before `apply`.

AI never sits on the deploy path. It can help you write the spec or diagnose a failed deploy, but what
ships is always the reviewed file.

## What anyship is, and isn't

anyship takes **an application from source to running** on a platform you pick, and helps you operate
it there (status, logs, diagnosis). It borrows Terraform's `plan` → `apply` → `destroy` flow because
reviewing a change before making it is the safe way to deploy, but it is **not infrastructure as
code**:

- **No state file.** anyship never records what it deployed. Every command asks the platform: Docker on
  the host, Cloudflare's API. There is nothing to lock, back up, import or drift from. What anyship
  writes under `.anyship/` is generated output for review, rebuilt on every deploy and never read back.
- **Applications, not infrastructure.** Servers, networks, DNS zones, accounts and managed databases
  are yours to provide (by hand, or with Terraform or OpenTofu). anyship deploys onto them and binds to
  what already exists, such as a D1 database id or an external Postgres URL.
- **One spec for every target.** `anyship.yaml` describes what the app needs, not how a provider builds
  it, so the same file deploys to a VPS or to Cloudflare. A target that can't provide a need says so.

If you need to create and track cloud resources, use Terraform or OpenTofu alongside anyship.

## What `init` detects

| Language | Detects | Start command |
|---|---|---|
| JavaScript / TypeScript | Next.js, Nuxt, SvelteKit, Astro, Hono, NestJS, Fastify, Express, Vite, CRA; npm, pnpm, yarn, bun | `start` script or `main` |
| Go | main package at the root or under `cmd/`; gin, fiber, echo, chi | `go build -o bin/<name>` → `./bin/<name>` |
| Python | Django, FastAPI, Flask, plain scripts; `requirements.txt` or `pyproject.toml` | gunicorn / uvicorn when listed, else the dev server (with a warning) |
| Rust | `[package]` / `[[bin]]`; actix-web, axum, rocket, warp, poem | `cargo build --release` → `./target/release/<bin>` |
| Anything else | a `Dockerfile`, or `index.html` for static sites | |

It also finds the listen port, database/cache/storage dependencies, and Node-only APIs that keep code off
edge runtimes, and warns when an app listens on `127.0.0.1` (unreachable from outside a container).

Container targets don't need you to write a Dockerfile: when a service has neither `image` nor
`dockerfile`, anyship generates one (multi-stage for Go and Rust, nginx for static sites) and shows it
in the plan. Commit your own Dockerfile whenever you want to take over.

## Quick start

anyship is a single binary with no runtime dependencies, for Linux, macOS and Windows on amd64 and
arm64.

```bash
curl -fsSL https://raw.githubusercontent.com/j75689/anyship/main/install.sh | sh
```

The script picks the archive for your OS and CPU from the
[latest release](https://github.com/j75689/anyship/releases/latest), checks it against the release's
`checksums.txt`, and installs to `/usr/local/bin` (or `~/.local/bin`). Set `ANYSHIP_VERSION=v0.2.0` to
pin a version or `ANYSHIP_INSTALL_DIR` to choose the directory. You can also download an archive from
the releases page, or build from source with Go 1.26+:
`go install github.com/j75689/anyship/cmd/anyship@latest`.

Then, in your project:

```bash
cd path/to/your/app
anyship init
anyship plan  --target cloudflare
anyship apply --target cloudflare --dry-run
```

The Cloudflare target runs `npx wrangler deploy`, so it needs Node.js, which any project you deploy
to Workers already has. Log in once with `npx wrangler login` (or set `CLOUDFLARE_API_TOKEN`).
Credentials stay on your machine; anyship has no server.

## The spec

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/j75689/anyship/main/schema/anyship.schema.json
apiVersion: anyship/v1alpha1
kind: App
metadata:
  name: my-api
spec:
  services:
    web:
      kind: server               # static | server | worker (background)
      entry: src/index.ts        # edge runtimes: module exporting fetch()
      start: node dist/server.js # process-based platforms
      ports:
        - port: 3000             # protocol: http|tcp|udp|tcp+udp, exposure: public|internal
      uses: [db]
      secrets: [JWT_SECRET]
      cron:
        - schedule: "0 * * * *"
  resources:
    db: {type: sqlite}           # postgres | mysql | sqlite | redis | bucket | kv
  secrets:
    JWT_SECRET: {generate: hex32}
  targets:
    cloudflare:
      bindings:
        db: {id: <d1-database-id>}
```

The layout follows Kubernetes manifests: `apiVersion` and `kind` say what the file is, `metadata.name`
names the app, and `spec` describes it. Editors with the YAML language server validate and
autocomplete against [`schema/anyship.schema.json`](schema/anyship.schema.json) through the first-line
comment, which `anyship init` writes (`anyship schema` prints the schema). `anyship validate` also checks cross-references, such as a service
using a resource that isn't declared, and rejects unknown fields so typos don't go unnoticed.

Coming from v0.2, where the spec was `anyship.json`? Only the envelope changed; everything under
`services`, `resources`, `secrets` and `targets` is the same, so `anyship migrate` converts the file
for you:

```bash
anyship migrate            # anyship.json -> anyship.yaml
anyship migrate --dry-run  # print it instead of writing it
```

It writes the envelope, keeps the rest as it is, validates the result, and refuses — with the field's
path and a suggestion — anything it doesn't recognize rather than dropping it. Your `anyship.json`
stays where it is until you delete it. [docs/MIGRATION.md](docs/MIGRATION.md) has the field-by-field
mapping and complete before/after examples.

## Targets

| Target | Status | Runs |
|---|---|---|
| `cloudflare` | ✅ v0.1 | Workers (edge handlers) and static assets; D1, KV, R2, Hyperdrive bindings; cron triggers; custom domains |
| `vps` | ✅ v0.2 | any Linux server with Docker, over SSH: images or Dockerfiles, multi-service, volumes, TCP/UDP ports, secrets |
| `gcp` | ✅ unreleased | Google Cloud Run: images or Dockerfiles (built locally, pushed to Artifact Registry), HTTP services, secrets in Secret Manager |
| `aws` | ✅ unreleased | Amazon ECS Express Mode: images or Dockerfiles (built locally, pushed to ECR), HTTP services with a managed load balancer and HTTPS URL, secrets in Secrets Manager |
| `fly` | planned | containers, volumes, Postgres |
| `vercel` | planned | static and serverless |
| Cloudflare Containers | planned | container images on Cloudflare |

### Cloudflare notes

- A spec maps to **one Worker**. Multi-service specs, container images, volumes and non-HTTP ports are
  rejected with a reason and a suggested alternative.
- `init` scans your code for Node-only APIs (`child_process`, native modules, ...) and marks the
  service `edgeCompatible: false`, so `plan` explains why it can't run on Workers before you deploy.
- D1, KV and Hyperdrive need existing resource ids. `plan` prints the `wrangler ... create` command
  to run and where to put the id. R2 buckets are addressed by name.
- Next.js needs `@opennextjs/cloudflare`; anyship doesn't drive it yet.

### VPS notes

```yaml
spec:
  targets:
    vps:
      host: deploy@203.0.113.10        # ssh destination or ~/.ssh/config alias
      port: 22                         # optional
      identityFile: ~/.ssh/id_ed25519  # optional
      dir: anyship/my-app              # optional; relative to the login user's home
      sudo: false                      # run docker via `sudo -n`
```

- The spec becomes a Docker Compose project (`.anyship/vps/compose.yaml`, kept locally for review).
  `apply` uploads it with the build context of every service that builds from source over `ssh`,
  then runs `docker compose up -d --build` on the host. The host needs Docker with the Compose plugin.
- Services without `image` or `dockerfile` are built from a generated Dockerfile
  (`.anyship/vps/<service>.Dockerfile`). Static sites are served by nginx on their public ports, or
  on port 80.
- anyship uses your system `ssh`, so `~/.ssh/config`, the agent, jump hosts and known_hosts checks
  all apply. It runs in batch mode: an unknown host key or a password prompt fails instead of
  hanging, so connect once with `ssh` first.
- `public` ports are published on the host (`tcp+udp` publishes both); `internal` ports are only
  reachable by other services, by service name.
- `start` runs as the container's command, exactly as written and without a shell; wrap it in
  `sh -c '...'` if you need pipes or variables.
- Secrets are mounted at `/run/secrets/<NAME>`. `generate: "hex32"` secrets are created on the host
  on first deploy and kept; others come from the same-named environment variable at `apply` time,
  or stay as set by a previous deploy.
- Every `apply` first runs preflight checks over ssh, and `apply --dry-run` stops after them:
  Docker Compose on the host must accept the generated `compose.yaml` (validated in a temporary
  directory that is removed afterwards), the host's architecture is reported (with a warning on
  arm64 for image-based services), free disk space is compared with the declared volume sizes, and
  published ports must be free unless the project is already running there. Nothing is uploaded or
  started until the checks pass.
- Not yet: cron, provisioning databases (declare them as services instead), domains and HTTPS, and
  generated Dockerfiles for languages other than JavaScript, Go, Python and Rust. `plan` explains
  each refusal.

### Google Cloud notes

```yaml
spec:
  targets:
    gcp:
      project: my-project   # Google Cloud project id
      region: us-central1
      repository: apps      # existing Artifact Registry Docker repository; needed to build from source
      private: false        # true: public services require authentication
```

- anyship drives your installed `gcloud` with its current login, and `docker buildx` for builds. It
  creates nothing outside Cloud Run and Secret Manager: create the Artifact Registry repository once
  (`gcloud artifacts repositories create apps --repository-format docker --location us-central1`).
- Each service becomes the Cloud Run service `<spec name>-<service>`, labeled
  `anyship-project=<spec name>`; `status`, `logs` and `destroy` find it by that label, with no local state.
- Services that build from source are built for `linux/amd64`, pushed, and deployed by digest, so a
  deploy runs exactly the image it built. Generated Dockerfiles are kept in `.anyship/gcp/` for review.
- Every `apply` first checks the login, the project, the required APIs (`run`, `artifactregistry`,
  `secretmanager`) and the repository; `apply --dry-run` stops after the checks.
- Secrets live in Secret Manager as `<spec name>-<NAME>` and reach the container as environment
  variables. A value in the deployer's environment adds a new version; `generate: "hex32"` secrets
  are created once and kept. `destroy --volumes` deletes them; images stay in the registry.
- One HTTP port per service (default 8080, also passed as `$PORT`); `internal` ports use internal
  ingress. `replicas` sets the minimum instance count. Services reach each other by URL, not by name.
- Refused with a reason: static sites, workers, volumes, cron, TCP/UDP ports and resources anyship
  would have to provision. `logs -f` points to `gcloud beta run services logs tail`.

### AWS notes

```yaml
spec:
  targets:
    aws:
      region: us-east-1
      profile: work                 # optional aws CLI profile
      cluster: default              # existing ECS cluster
      repository: apps              # existing ECR repository; needed to build from source
      executionRole: ecsTaskExecutionRole                       # name or ARN (default shown)
      infrastructureRole: ecsInfrastructureRoleForExpressServices # name or ARN (default shown)
      taskRole: my-app-role         # optional, for the app's own AWS access
      subnets: [subnet-0abc]        # optional; default: the default VPC's public subnets
      securityGroups: [sg-0abc]     # optional, with subnets
      cpu: "1024"                   # optional, per task
      memory: "2048"
      maxTasks: 4                   # autoscaling ceiling (at least replicas)
```

- anyship drives your installed `aws` CLI (v2, with ECS Express Mode support) with its current login,
  and `docker buildx` for builds. App Runner no longer takes new customers, so services run on
  [ECS Express Mode](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/express-service-overview.html),
  which provisions the load balancer, HTTPS URL (`<service>.ecs.<region>.on.aws`) and autoscaling.
- Create once per account and region: the cluster, the ECR repository and the two IAM roles from
  AWS's [Express Mode guide](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/express-service-getting-started.html).
  `apply` checks them first and prints the commands for anything missing; `apply --dry-run` stops after the checks.
- Each service becomes the Express Mode service `<spec name>-<service>`, tagged
  `anyship-project=<spec name>`; `status`, `logs` and `destroy` find it by name, with no local state.
  Built images are tagged with the service name in the repository and deployed by digest.
- Secrets live in Secrets Manager as `anyship/<spec name>/<NAME>` and reach the container as environment
  variables. The execution role needs `secretsmanager:GetSecretValue` on them (`plan` warns). Values are
  handed to the aws CLI through a private temporary file, never on the command line.
- The load balancer checks `healthCheck.path`, or `/`, for HTTP 200. One HTTP port per service
  (default 80); `internal` ports need private `subnets`. `replicas` is the minimum task count.
- `logs` uses `aws logs tail`, so `-f` works for one service at a time; it selects by `--since`, not `-n`.
- `destroy` deletes the services with their load balancers; the cluster, roles, images and log groups
  stay. `destroy --volumes` also deletes the secrets without a recovery window.
- Refused with a reason: static sites, workers, volumes, cron, TCP/UDP ports and resources anyship
  would have to provision.
- This target has not been run against a real AWS account yet. Express Mode provisions a load
  balancer that bills whether or not it serves traffic, so read
  [docs/verified/aws.md](docs/verified/aws.md) — the runbook and residual-resource checklist — before
  you point it at an account you pay for.

## Logs

`anyship logs` reads runtime logs from where a spec is deployed, using the same target settings:

```console
$ anyship logs -t vps                          # last 100 lines of every service
$ anyship logs -t vps reth -f                  # follow one service
$ anyship logs -t vps lighthouse --since 10m -n 500 --timestamps
$ anyship logs -t cloudflare                   # live Worker logs via wrangler tail
```

On `vps` this runs `docker compose logs` on the host over ssh. On `cloudflare` it streams live logs
with `wrangler tail`; Workers keep no history that wrangler can read, so `--tail` and `--since` are
refused there (turn on Workers Logs in the Cloudflare dashboard for history).

## Status and destroy

```console
$ anyship status -t vps
eth-mainnet on vps (deploy@203.0.113.10:anyship/eth-mainnet)
  SERVICE     STATE    HEALTH   RUNNING  PORTS                 DETAIL
  lighthouse  running  -        1/1      9000/tcp, 9000/udp    Up 2 hours
  reth        running  healthy  1/1      30303/tcp, 30303/udp  Up 2 hours (healthy)
All services are running.

$ anyship destroy -t vps             # stop and remove containers; keep volumes and secrets
$ anyship destroy -t vps --volumes   # also delete data (asks you to type the project name)
$ anyship destroy -t vps --dry-run   # show what would go and what runs now; remove nothing
```

`status` exits 1 when the spec isn't deployed or a service isn't fully running, so it works as a
health check in scripts. It isn't available on `cloudflare` yet. `destroy` on `cloudflare` deletes the
Worker with `wrangler delete` and never touches bound D1, KV, R2 or Hyperdrive resources.

`plan`, `status` and `targets` take `--json` for scripts and agents; progress messages go to stderr.

## Diagnose failures with Claude

```console
$ anyship diagnose -t vps --note "lighthouse keeps restarting"
# Illustrative output:
Diagnosis: lighthouse can't authenticate to reth's engine API.

Root cause (high confidence)
...
Proposed change to anyship.yaml (applies cleanly and plans without errors)
  add /services/lighthouse/secrets: ["JWT_SECRET"]

Apply this change to anyship.yaml? [y/N]
```

`diagnose` collects what anyship already knows (plan findings, the target's dry-run checks, status,
recent logs and the generated compose.yaml or Dockerfiles) and asks Claude for the root cause, the
evidence for it, next steps, and a fix. The rules:

- **Nothing is written unless you confirm.** A proposed `anyship.yaml` change is shown only if it
  applies and the patched spec plans cleanly; otherwise the rejection goes back to Claude, up to three
  times. Fixes outside `anyship.yaml` (your code, a hand-written Dockerfile, the host) are described,
  never applied.
- **Secrets are redacted before anything is sent:** values of the spec's secrets found in your
  environment, credential-like keys (`*_KEY`, `*_TOKEN`, `*PASSWORD*`, ...), API tokens, passwords in
  URLs and private keys. `--show-context` prints exactly what would be sent, without calling the API.
- **Bring your own credentials:** `ANTHROPIC_API_KEY`, or `ant auth login`. It uses `claude-opus-5-5`
  at high effort by default (`--model`, `--effort` to change). Everything else in anyship works without
  a key.

Using an agent instead? The MCP tool `diagnose_context` returns the same redacted context, so the agent
can diagnose with its own model and no extra key.

## Use it from AI agents (MCP)

`anyship mcp` serves anyship over the [Model Context Protocol](https://modelcontextprotocol.io) on
stdio, so coding agents can detect, plan, deploy and debug for you. With Claude Code:

```bash
claude mcp add anyship -- anyship mcp                  # plan, dry runs, status, logs
claude mcp add anyship -- anyship mcp --allow-deploy   # also real deploys and destroys
claude mcp list                                        # anyship: … - ✔ Connected
```

`anyship` has to be on `PATH` when Claude Code starts, because Claude Code runs the command itself.
The server also inherits the directory Claude Code was started in, and that is what `config` and `dir`
fall back to. In a monorepo, tell the agent which subdirectory the app is in, or `detect` reads the
repo root and finds nothing to build.

| Tool | Does | Writes files |
|---|---|---|
| `targets` | list targets and what each supports | no |
| `detect` | draft an `anyship.yaml` for a directory (returned, not written) | no |
| `validate` | check `anyship.yaml` | no |
| `plan` | what a deploy would do, and every unmet need | no |
| `status`, `logs` | what runs on the target, and its recent logs | no |
| `diagnose_context` | everything above plus dry-run checks, redacted, for diagnosing a failure | `.anyship/<target>/` |
| `apply` | deploy; `dry_run` runs the target's checks and generates the deployment files | `.anyship/<target>/`; deploys only with `--allow-deploy` |
| `destroy` | remove a deployment; `volumes` also deletes data | no; removes only with `--allow-deploy` |

Running the target's checks means generating the compose file and Dockerfiles first, so
`diagnose_context` and `apply` with `dry_run` both leave `.anyship/<target>/` behind for review.
Neither one touches the platform.

Safety is built into the server, not left to the agent: without `--allow-deploy` only dry runs are
possible, and deleting data needs `confirm_project` set to the spec's name. Tools carry read-only and
destructive hints so hosts can ask you before risky calls. Output from ssh, wrangler and builds is
returned in the tool result; nothing else touches the protocol's stdin and stdout.

[docs/verified/mcp.md](docs/verified/mcp.md) records a full run from Claude Code, including where an
agent gets stuck.

## Examples

- [`examples/hono-worker`](examples/hono-worker): a Hono app that deploys to Cloudflare Workers.
- [`examples/ethereum-node`](examples/ethereum-node): a Reth + Lighthouse Ethereum node, used to
  stress the spec (2 TB NVMe volumes, P2P TCP/UDP ports, shared JWT secret). The `vps` target deploys
  it; the Cloudflare target refuses it and says why.

## Roadmap

- **v0.2** ✅ `vps` adapter, ✅ MCP server. Next: domains and HTTPS on `vps`, release binaries,
  Fly.io adapter.
- **v0.3** AI layer (bring your own key): ✅ `diagnose` for failed deploys with validated spec fixes.
  Next: `init --ai` to draft specs for unrecognized stacks, and an AI review before deploying.
- **v0.4** Cloudflare Containers, Vercel adapter, and creating app-scoped resources (such as a D1
  database) during `apply` when missing, found by name on the platform rather than tracked in state.
- **Next** ✅ Google Cloud Run and ✅ AWS (ECS Express Mode) adapters.
- **Later** community adapters (Railway), recipes with parameters (e.g. N-node RPC clusters).

## Project layout

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the system map and the flows.

```
cmd/anyship/           main package
spec/                  anyship.yaml types, validation, JSON Schema
adapter/               adapter contract and registry
adapters/cloudflare/   Cloudflare Workers adapter
adapters/vps/          Docker Compose over SSH adapter
adapters/gcp/          Google Cloud Run adapter
adapters/aws/          Amazon ECS Express Mode adapter
image/                 docker buildx build --push, by digest
detect/                rule-based project detection (one file per language)
dockerfile/            Dockerfile generation for services that have none
internal/cli/          the `anyship` command (cobra)
examples/              sample specs
schema/                generated JSON Schema
```

## Releases

[CHANGELOG.md](CHANGELOG.md) lists what changed in each version, including breaking changes.
[docs/MIGRATION.md](docs/MIGRATION.md) covers the move from v0.2 (`anyship.json`) to v0.3
(`anyship.yaml`).

## Contributing

Adapters are the easiest way to help. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[Apache-2.0](LICENSE)
