# anyship

**Describe your app once. Deploy it anywhere.**

anyship reads your project, drafts a platform-neutral deploy spec (`anyship.json`), and turns that one
spec into a deployment on whichever platform you pick. Each platform adapter either satisfies every
need in the spec or tells you exactly which need it can't meet and why. It never quietly drops one.

> **Status: early (v0.2).** The spec, rule-based detection, the CLI, a Cloudflare Workers adapter and a
> VPS (Docker over SSH) adapter work today. More targets and the AI assistant layer are on the [roadmap](#roadmap).

```console
$ anyship init
Detected:
  • package manager: npm
  • dependency "hono" → hono (server)
  • edge entry module: src/index.ts
✔ wrote anyship.json

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

1. **Understand.** Detect the stack and draft `anyship.json`. Rule-based and deterministic; an optional AI
   layer (planned) only *proposes* edits to the spec for you to review.
2. **Describe.** `anyship.json` is the single source of truth: services, ports, disks, databases, secrets.
   You commit it, and every deploy reads it, so deploys are reproducible.
3. **Deliver.** Adapters translate the spec into platform config deterministically, with a
   Terraform-style `plan` before `apply`.

AI never sits on the deploy path. It can help you write the spec or diagnose a failed deploy, but what
ships is always the reviewed file.

## Quick start

anyship is a single Go binary with no runtime dependencies. Release binaries aren't published yet, so
install from source (Go 1.26+):

```bash
go install github.com/j75689/anyship/cmd/anyship@latest

cd path/to/your/app
anyship init
anyship plan  --target cloudflare
anyship apply --target cloudflare --dry-run
```

The Cloudflare target runs `npx wrangler deploy`, so it needs Node.js, which any project you deploy
to Workers already has. Log in once with `npx wrangler login` (or set `CLOUDFLARE_API_TOKEN`).
Credentials stay on your machine; anyship has no server.

## The spec

```jsonc
{
  "version": 1,
  "name": "my-api",
  "services": {
    "web": {
      "kind": "server",              // static | server | worker (background)
      "entry": "src/index.ts",       // edge runtimes: module exporting fetch()
      "start": "node dist/server.js",// process-based platforms
      "ports": [{ "port": 3000 }],   // protocol: http|tcp|udp|tcp+udp, exposure: public|internal
      "uses": ["db"],
      "secrets": ["JWT_SECRET"],
      "cron": [{ "schedule": "0 * * * *" }]
    }
  },
  "resources": { "db": { "type": "sqlite" } },   // postgres | mysql | sqlite | redis | bucket | kv
  "secrets": { "JWT_SECRET": { "generate": "hex32" } },
  "targets": {
    "cloudflare": { "bindings": { "db": { "id": "<d1-database-id>" } } }
  }
}
```

Editors can validate and autocomplete against [`schema/anyship.schema.json`](schema/anyship.schema.json)
(`anyship schema` prints it). `anyship validate` also checks cross-references, such as a service
using a resource that isn't declared, and rejects unknown fields so typos don't go unnoticed.

## Targets

| Target | Status | Runs |
|---|---|---|
| `cloudflare` | ✅ v0.1 | Workers (edge handlers) and static assets; D1, KV, R2, Hyperdrive bindings; cron triggers; custom domains |
| `vps` | ✅ v0.2 | any Linux server with Docker, over SSH: images or Dockerfiles, multi-service, volumes, TCP/UDP ports, secrets |
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

```jsonc
"targets": {
  "vps": {
    "host": "deploy@203.0.113.10", // ssh destination or ~/.ssh/config alias
    "port": 22,                     // optional
    "identityFile": "~/.ssh/id_ed25519", // optional
    "dir": "anyship/my-app",        // optional; relative to the login user's home
    "sudo": false                   // run docker via `sudo -n`
  }
}
```

- The spec becomes a Docker Compose project (`.anyship/vps/compose.yaml`, kept locally for review).
  `apply` uploads it with each Dockerfile service's build context over `ssh`, then runs
  `docker compose up -d --build` on the host. The host needs Docker with the Compose plugin.
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
- `apply --dry-run` only checks that the host is reachable and has Docker Compose.
- Not yet: static-only services, services without an image or Dockerfile, cron, provisioning
  databases (declare them as services instead), domains and HTTPS. `plan` explains each refusal.

## Examples

- [`examples/hono-worker`](examples/hono-worker): a Hono app that deploys to Cloudflare Workers.
- [`examples/ethereum-node`](examples/ethereum-node): a Reth + Lighthouse Ethereum node, used to
  stress the spec (2 TB NVMe volumes, P2P TCP/UDP ports, shared JWT secret). The `vps` target deploys
  it; the Cloudflare target refuses it and says why.

## Roadmap

- **v0.2** ✅ `vps` adapter. Next: domains and HTTPS on `vps`, Fly.io adapter, MCP server so coding
  agents can drive anyship.
- **v0.3** AI layer (bring your own model and key): draft specs for unrecognized stacks, Workers
  compatibility review, failed-deploy diagnosis that proposes spec diffs.
- **v0.4** Cloudflare Containers, resource creation during `apply`, Vercel adapter.
- **Later** community adapters (Railway, Cloud Run, AWS), recipes with parameters (e.g. N-node RPC clusters).

## Project layout

```
cmd/anyship/           main package
spec/                  anyship.json types, validation, JSON Schema
adapter/               adapter contract and registry
adapters/cloudflare/   Cloudflare Workers adapter
adapters/vps/          Docker Compose over SSH adapter
detect/                rule-based project detection
internal/cli/          the `anyship` command (cobra)
examples/              sample specs
schema/                generated JSON Schema
```

## Contributing

Adapters are the easiest way to help. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[Apache-2.0](LICENSE)
