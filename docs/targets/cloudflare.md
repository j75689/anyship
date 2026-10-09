# The `cloudflare` target

Deploys edge handlers and static sites to Cloudflare Workers by running `npx wrangler deploy`, so it
needs Node.js, wrangler 3.91 or newer (the first to read `wrangler.jsonc` without a flag) and a
`wrangler login` (or `CLOUDFLARE_API_TOKEN`). The generated config is kept in
`.anyship/cloudflare/wrangler.jsonc` for review.

- Every `apply` first checks that Node.js and npx are installed, that wrangler runs and that it is
  logged in (`wrangler whoami`), before the build runs (`CF_PREFLIGHT_NODE`, `CF_PREFLIGHT_WRANGLER`,
  `CF_PREFLIGHT_AUTH`). `apply --dry-run` needs no login, so there a missing one is only a warning.
  The checks never download wrangler: until npx has it, they are skipped and the deploy's npx
  downloads it.
- A spec maps to **one Worker**. Multi-service specs, container images, volumes and non-HTTP ports are
  rejected with a reason and a suggested alternative.
- `init` scans your code for Node-only APIs (`child_process`, native modules, ...) and marks the
  service `edgeCompatible: false`, so `plan` explains why it can't run on Workers before you deploy.
- D1, KV and Hyperdrive need existing resource ids. `plan` prints the `wrangler ... create` command
  to run and where to put the id. R2 buckets are addressed by name.
- `services.<name>.domains` become custom-domain routes in the generated `wrangler.jsonc`. The zone
  must already be on your Cloudflare account; Cloudflare issues the certificate.
  `targets.cloudflare.domains` overrides the service's list (see [Domains](../../README.md#domains)).
- `${services.<name>.url}` in `env` is refused (`CF_SERVICE_URL`): a Worker's URL depends on the
  account's `workers.dev` subdomain or a custom domain. Put it in `env` by hand.
- `cron` schedules become cron triggers, which call the Worker's `scheduled()` handler. A `path` or
  a `command` in the entry isn't used, and `plan` warns about it (`CF_CRON_PATH`, `CF_CRON_COMMAND`).
- `memory` and `cpu` aren't applied, and `plan` warns about it (`CF_RESOURCES_IGNORED`): every
  Worker gets the same limits, 128 MB of memory among them.
- Next.js needs `@opennextjs/cloudflare`; anyship doesn't drive it yet.

[All targets](../../README.md#targets)
