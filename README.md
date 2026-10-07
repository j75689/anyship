<p align="center">
  <img src="docs/assets/banner.svg" width="100%" alt="anyship: describe your app once, deploy it anywhere. One anyship.yaml is deployed to Cloudflare, a VPS, Google Cloud or AWS, with more platforms to come.">
</p>

# anyship: describe your app once, deploy it anywhere

anyship is an open source CLI that reads your project, drafts a platform-neutral deploy spec
(`anyship.yaml`), and turns that one spec into a deployment on whichever platform you pick: Cloudflare,
a VPS of your own, Google Cloud or AWS today, and more platforms as adapters are added. Each adapter
either satisfies every need in the spec or tells you exactly which need it can't meet and why. It
never quietly drops one.

anyship is a single Go binary. It drives the tools you already use (`wrangler`, `ssh`, `gcloud`,
`aws`) with your own logins, has no server, and keeps no state.

[![Release](https://img.shields.io/github/v/release/j75689/anyship)](https://github.com/j75689/anyship/releases/latest)
[![CI](https://github.com/j75689/anyship/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/j75689/anyship/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/j75689/anyship)](go.mod)
[![License](https://img.shields.io/github/license/j75689/anyship)](LICENSE)
[![M8ven Score](https://m8ven.ai/badge/mcp/j75689/anyship)](https://m8ven.ai/mcp/j75689/anyship?s=readme)

> **Status: early (v0.3).** The spec, rule-based detection, the CLI, the MCP server and four targets
> are implemented: Cloudflare Workers, VPS (Docker over SSH), Google Cloud Run and Amazon ECS Express
> Mode. The [Targets](#targets) table says which of them have been run against the real platform.
> `diagnose` is experimental. What comes next is on the [roadmap](#roadmap).

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
`checksums.txt`, and installs to `/usr/local/bin` (or `~/.local/bin`). Set `ANYSHIP_VERSION=v0.4.0` to
pin a version or `ANYSHIP_INSTALL_DIR` to choose the directory. You can also download an archive from
the releases page, or build from source with Go 1.27+:
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
      memory: 512MB              # what one instance needs: 512MB, 4GB, ...
      cpu: 1                     # ... and how many CPUs: 0.5, 1, 2, ...
      ports:
        - port: 3000             # protocol: http|tcp|udp|tcp+udp, exposure: public|internal
      domains: [api.example.com] # custom domains for a public http port
      env:
        API_URL: ${services.api.url}  # another service's URL on whichever target deploys this
      uses: [db]
      secrets: [JWT_SECRET]
      cron:
        - schedule: "0 * * * *"  # with path: /tick (an HTTP call to the service) or command: ./job
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

### Domains

`services.<name>.domains` names the hosts a service answers on, in one place for every target:

```yaml
spec:
  services:
    web:
      kind: server
      ports:
        - port: 3000             # protocol: http, exposure: public
      domains:
        - example.com
        - www.example.com
```

The rules `validate` enforces:

- A domain is a fully qualified name such as `app.example.com`. No scheme, port, path or wildcard;
  upper case and a trailing dot are accepted and normalized away.
- A service with domains needs at least one port with `protocol: http` and `exposure: public`, since
  that is the port a platform's HTTP router sends the traffic to.
- A domain belongs to one service: the same host twice in one app is an error.

Only the `cloudflare` target attaches domains today. The others say so with an error finding
(`VPS_DOMAIN_UNSUPPORTED`, `GCP_DOMAIN_UNSUPPORTED`, `AWS_DOMAIN_UNSUPPORTED`) and a hint, rather
than deploying a service that answers on the wrong host. anyship never issues TLS certificates or
creates DNS records; the platform or you do that.

`targets.cloudflare.domains` still works and **replaces** `services.<name>.domains` for that target
when it is set, with a `CF_DOMAIN_OVERRIDE` note in the plan. The override wins so one spec can go to
Cloudflare on staging hosts, and merging the two lists would make it impossible to deploy fewer
domains than the spec asks for. Remove `targets.cloudflare.domains` to use the service's own list.

## Targets

| Target | Since | Run against the real platform | Runs |
|---|---|---|---|
| [`cloudflare`](docs/targets/cloudflare.md) | v0.1 | not since the v0.3 manifest change ([#28](https://github.com/j75689/anyship/issues/28)) | Workers (edge handlers) and static assets; D1, KV, R2, Hyperdrive bindings; cron triggers; custom domains |
| [`vps`](docs/targets/vps.md) | v0.2 | ✅ on every pull request, in CI | any Linux server with Docker, over SSH: images or Dockerfiles, multi-service, volumes, TCP/UDP ports, secrets |
| [`gcp`](docs/targets/gcp.md) | v0.3 | ✅ by hand, on a real project | Google Cloud Run: images or Dockerfiles (built locally, pushed to Artifact Registry), HTTP services, secrets in Secret Manager |
| [`aws`](docs/targets/aws.md) | v0.3 | ⚠ not yet; tested with a fake `aws` CLI ([#26](https://github.com/j75689/anyship/issues/26)) | Amazon ECS Express Mode: images or Dockerfiles (built locally, pushed to ECR), HTTP services with a managed load balancer and HTTPS URL, secrets in Secrets Manager |
| `fly` | planned | | containers, volumes, Postgres |
| `vercel` | planned | | static and serverless |
| Cloudflare Containers | planned | | container images on Cloudflare |

Each target's page, linked from its name, lists the options it takes, how it deploys and what it
refuses.

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
`plan --json` includes the contents of the files a deploy would generate, since `plan` writes none.

`plan` and `apply` take `--image <service>=<image>` to deploy a different image than the spec names,
for that run only. A pipeline passes the digest it just built, and `anyship.yaml` stays as it is:

```console
$ anyship apply -t gcp --image web=us-docker.pkg.dev/acme/apps/shop@sha256:9b2c…
```

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

> **Experimental.** `diagnose` is tested against a stubbed API and has not been run against the live
> Claude API yet ([#27](https://github.com/j75689/anyship/issues/27)). Redaction works by pattern, so
> it can miss a secret in a shape it doesn't know: `--show-context` needs no key and prints exactly
> what would be sent, and it is worth searching that for your own secrets before the first real call.

`diagnose` collects what anyship already knows (plan findings, the target's dry-run checks, status,
recent logs and the generated compose.yaml or Dockerfiles) and asks Claude for the root cause, the
evidence for it, next steps, and a fix. The rules:

- **Nothing is written unless you confirm.** A proposed `anyship.yaml` change is shown only if it
  applies and the patched spec plans cleanly; otherwise the rejection goes back to Claude, up to three
  times. Fixes outside `anyship.yaml` (your code, a hand-written Dockerfile, the host) are described,
  never applied.
- **Secrets are redacted before anything is sent:** values of the spec's secrets found in your
  environment, credential-like keys (`*_KEY`, `*_TOKEN`, `*PASSWORD*`, ...), `Authorization` headers,
  API tokens, passwords in URLs and private keys. `--show-context` prints exactly what would be sent,
  without calling the API.
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
fall back to. It tells the agent which directory that is, and reports a missing `anyship.yaml` by the
full path it tried. In a repository with several apps, `detect` at the root names the subdirectories
that look like projects, so the agent can pass one as `dir`.

| Tool | Does | Writes files |
|---|---|---|
| `targets` | list targets and what each supports | no |
| `detect` | draft an `anyship.yaml` for a directory (returned, not written) | no |
| `validate` | check `anyship.yaml` | no |
| `plan` | what a deploy would do, every unmet need, and the generated files with their contents | no |
| `status`, `logs` | what runs on the target, and its recent logs | no |
| `diagnose_context` | everything above plus dry-run checks, redacted, for diagnosing a failure | no |
| `apply` | deploy; `dry_run` runs the target's checks and generates the deployment files | `.anyship/<target>/`; deploys only with `--allow-deploy` |
| `destroy` | remove a deployment; `volumes` also deletes data | no; removes only with `--allow-deploy` |

The read-only tools leave your project alone, so a host can run them without asking: `plan` returns
the compose file and any generated Dockerfile with their contents instead of writing them, and
`diagnose_context` runs the target's checks in a temporary directory. Only `apply` writes
`.anyship/<target>/`, dry run or not, and a dry run deploys nothing.

Safety is built into the server, not left to the agent: without `--allow-deploy` only dry runs are
possible, and deleting data needs `confirm_project` set to the spec's name. Tools carry read-only and
destructive hints so hosts can ask you before risky calls. Output from ssh, wrangler and builds is
returned in the tool result; nothing else touches the protocol's stdin and stdout.

## Examples

- [`examples/hono-worker`](examples/hono-worker): a Hono app that deploys to Cloudflare Workers.
- [`examples/ethereum-node`](examples/ethereum-node): a Reth + Lighthouse Ethereum node, used to
  stress the spec (2 TB NVMe volumes, P2P TCP/UDP ports, shared JWT secret). The `vps` target deploys
  it; the Cloudflare target refuses it and says why.

## Roadmap

The roadmap is the issue list, so it can't fall behind the work:

- [`enhancement`](https://github.com/j75689/anyship/labels/enhancement) issues are what comes next.
- [`verification`](https://github.com/j75689/anyship/labels/verification) issues are the targets and
  features that still need a run against the real platform.

## Releases

[CHANGELOG.md](CHANGELOG.md) lists what changed in each version, including breaking changes.

## Contributing

Adapters are the easiest way to help. See [CONTRIBUTING.md](CONTRIBUTING.md) and the
[code of conduct](CODE_OF_CONDUCT.md); [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) has the system
map, the packages and the main flows.

Found a security problem? Report it privately; [SECURITY.md](SECURITY.md) says how.

## License

[Apache-2.0](LICENSE). Copyright 2026 The anyship Authors.
