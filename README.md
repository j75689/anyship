# anyship

**Describe your app once. Deploy it anywhere.**

anyship reads your project, drafts a platform-neutral deploy spec (`anyship.json`), and turns that one
spec into a deployment on whichever platform you pick. Each platform adapter either satisfies every
need in the spec or tells you exactly which need it can't meet and why. It never quietly drops one.

> **Status: early (v0.1).** The spec, rule-based detection, the CLI and a Cloudflare Workers adapter
> work today. More targets and the AI assistant layer are on the [roadmap](#roadmap).

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

Requires Node.js 22+. Packages aren't published to npm yet, so run from source:

```bash
git clone <this repo> anyship && cd anyship
npm install
npm run anyship -- init path/to/your/app
npm run anyship -- plan  --target cloudflare -c path/to/your/app/anyship.json
npm run anyship -- apply --target cloudflare -c path/to/your/app/anyship.json --dry-run
```

`apply` runs `npx wrangler deploy`, so log in once with `npx wrangler login` (or set
`CLOUDFLARE_API_TOKEN`). Credentials stay on your machine; anyship has no server.

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
(regenerate with `npm run schema`). `anyship validate` also checks cross-references, such as a service
using a resource that isn't declared.

## Targets

| Target | Status | Runs |
|---|---|---|
| `cloudflare` | ✅ v0.1 | Workers (edge handlers) and static assets; D1, KV, R2, Hyperdrive bindings; cron triggers; custom domains |
| `vps` (Docker over SSH) | planned | containers, volumes, TCP/UDP ports, multi-service |
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

## Examples

- [`examples/hono-worker`](examples/hono-worker): a Hono app that deploys to Cloudflare Workers.
- [`examples/ethereum-node`](examples/ethereum-node): a Reth + Lighthouse Ethereum node, used to
  stress the spec (2 TB NVMe volumes, P2P TCP/UDP ports, shared JWT secret). The Cloudflare adapter
  correctly refuses it; the planned `vps` adapter is its real target.

## Roadmap

- **v0.2** `vps` adapter (Docker over SSH), Fly.io adapter, MCP server so coding agents can drive anyship.
- **v0.3** AI layer (bring your own model and key): draft specs for unrecognized stacks, Workers
  compatibility review, failed-deploy diagnosis that proposes spec diffs.
- **v0.4** Cloudflare Containers, resource creation during `apply`, Vercel adapter.
- **Later** community adapters (Railway, Cloud Run, AWS), recipes with parameters (e.g. N-node RPC clusters).

## Project layout

```
packages/
  spec/                anyship.json schema (zod), types, loader
  core/                adapter contract, registry, rule-based detection
  adapter-cloudflare/  Cloudflare Workers adapter
  cli/                 `anyship` command
examples/              sample specs
schema/                generated JSON Schema
```

## Contributing

Adapters are the easiest way to help. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[Apache-2.0](LICENSE)
