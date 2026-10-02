# Migrating from v0.2 to v0.3

v0.3 replaces `anyship.json` with `anyship.yaml`, a Kubernetes-style manifest. A v0.2 file does not
load in v0.3, and `anyship` says so instead of guessing:

```
anyship.yaml not found, but anyship.json is: specs are YAML manifests now
(apiVersion: anyship/v1alpha1); run `anyship migrate` to convert it, or read
docs/MIGRATION.md: https://github.com/j75689/anyship/blob/main/docs/MIGRATION.md
```

**Only the envelope changed.** Everything under `services`, `resources`, `secrets` and `targets` is
byte-for-byte the same as in v0.2: the same keys, the same values, the same defaults, the same
validation. You rename the file, add three lines at the top, and indent the rest by one level.

## What moved

| v0.2 (`anyship.json`) | v0.3 (`anyship.yaml`) |
|---|---|
| file name `anyship.json` | file name `anyship.yaml` |
| `"$schema": "…/anyship.schema.json"` | first-line comment `# yaml-language-server: $schema=…/anyship.schema.json` |
| `"version": 1` | `apiVersion: anyship/v1alpha1` and `kind: App` |
| `"name": "shop"` | `metadata.name: shop` |
| `"services": {…}` | `spec.services: {…}` |
| `"resources": {…}` | `spec.resources: {…}` |
| `"secrets": {…}` | `spec.secrets: {…}` |
| `"targets": {…}` | `spec.targets: {…}` |

`apiVersion` and `kind` are required and must be exactly `anyship/v1alpha1` and `App`. Unknown fields
are still rejected, so a leftover `version:` or a top-level `name:` is an error, not a warning.

## What did not move

Nothing else. These all keep the names and the meanings they had in v0.2:

- services: `kind`, `path`, `image`, `dockerfile`, `build`, `start`, `entry`, `ports`, `volumes`,
  `replicas`, `env`, `secrets`, `uses`, `cron`, `runtime`, `healthCheck`, `dependsOn`
- resources: `type`, `external`
- secrets: `generate`, `description`
- targets: each adapter's own block, unchanged

## How to migrate

Run `anyship migrate` in the directory that holds `anyship.json`:

```bash
anyship migrate              # writes anyship.yaml next to anyship.json
anyship migrate --dry-run    # prints the manifest instead of writing it
anyship migrate --force      # replaces an anyship.yaml that is already there
anyship migrate path/to/dir  # migrates another directory
```

It applies exactly the table above: your `services`, `resources`, `secrets` and `targets` move under
`spec` unchanged, and the envelope is written for you, schema comment included. It then validates the
result and prints any problem the spec already had, so you fix it in the new format rather than the
old one. `anyship.json` is left alone; delete it once you are happy with `anyship.yaml`.

`migrate` converts `version: 1` files only, and refuses anything it does not recognize instead of
dropping it:

```
error: anyship.json cannot be migrated:
  ✖ region: unknown field; known fields are $schema, version, name, services, resources, secrets, targets
  ✖ spec.services.web.healthcheck: unknown field; did you mean "healthCheck"?
```

A field anyship never had is a field it never deployed, so fix the typo (or delete the field) and run
`migrate` again. The only thing `migrate` does not keep is the order of your keys: it writes them in
the canonical order, the same one `anyship init` uses.

### By hand

The conversion is small enough to do in an editor:

1. `git mv anyship.json anyship.yaml`.
2. Delete `$schema`, `version` and `name` from the top of the file.
3. Add the envelope: the schema comment, `apiVersion`, `kind`, and `metadata.name` with the name you
   deleted in step 2.
4. Put the rest under `spec:`, indented one level. JSON is valid YAML, so you can keep the JSON
   syntax inside `anyship.yaml` if you would rather not reformat; `anyship` reads both.
5. Run `anyship validate`.

`anyship init --force` also writes a v0.3 file, but it drafts one from project detection rather than
converting yours, so it drops anything you wrote by hand. Use it for a spec you never edited.

## A minimal spec

Before, `anyship.json`:

```json
{
  "version": 1,
  "name": "hono-worker",
  "services": {
    "web": {
      "kind": "server",
      "entry": "src/index.ts",
      "env": { "GREETING": "hello" },
      "runtime": { "language": "javascript", "framework": "hono", "edgeCompatible": true }
    }
  }
}
```

After, `anyship.yaml`:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/j75689/anyship/main/schema/anyship.schema.json
apiVersion: anyship/v1alpha1
kind: App
metadata:
  name: hono-worker
spec:
  services:
    web:
      kind: server
      entry: src/index.ts
      env:
        GREETING: hello
      runtime:
        language: javascript
        framework: hono
        edgeCompatible: true
```

## A spec that uses every top-level block

Before, `anyship.json`:

```json
{
  "$schema": "https://raw.githubusercontent.com/j75689/anyship/main/schema/anyship.schema.json",
  "version": 1,
  "name": "shop",
  "services": {
    "web": {
      "kind": "server",
      "path": "apps/web",
      "start": "node dist/server.js",
      "ports": [{ "port": 3000 }],
      "env": { "LOG_LEVEL": "info" },
      "uses": ["db"],
      "secrets": ["JWT_SECRET"],
      "healthCheck": { "path": "/healthz" },
      "replicas": 2
    },
    "mailer": {
      "kind": "worker",
      "path": "apps/mailer",
      "start": "node dist/worker.js",
      "uses": ["cache"],
      "cron": [{ "schedule": "0 * * * *", "command": "node dist/digest.js" }],
      "dependsOn": ["web"]
    }
  },
  "resources": {
    "db": { "type": "postgres" },
    "cache": { "type": "redis", "external": true }
  },
  "secrets": {
    "JWT_SECRET": { "generate": "hex32", "description": "Signs session tokens" }
  },
  "targets": {
    "vps": { "host": "deploy@203.0.113.10" }
  }
}
```

After, `anyship.yaml`:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/j75689/anyship/main/schema/anyship.schema.json
apiVersion: anyship/v1alpha1
kind: App
metadata:
  name: shop
spec:
  services:
    web:
      kind: server
      path: apps/web
      start: node dist/server.js
      ports:
        - port: 3000
      env:
        LOG_LEVEL: info
      uses: [db]
      secrets: [JWT_SECRET]
      healthCheck:
        path: /healthz
      replicas: 2
    mailer:
      kind: worker
      path: apps/mailer
      start: node dist/worker.js
      uses: [cache]
      cron:
        - schedule: "0 * * * *"
          command: node dist/digest.js
      dependsOn: [web]
  resources:
    db:
      type: postgres
    cache:
      type: redis
      external: true
  secrets:
    JWT_SECRET:
      generate: hex32
      description: Signs session tokens
  targets:
    vps:
      host: deploy@203.0.113.10
```

Both "after" specs in this file are parsed by the test suite (`TestMigrationGuideExamples`), and both
"before" specs are checked to be rejected, so the guide cannot drift from the code.

## More examples

[`examples/hono-worker`](../examples/hono-worker/anyship.yaml) and
[`examples/ethereum-node`](../examples/ethereum-node/anyship.yaml) are complete v0.3 specs. The
[spec reference](../README.md#the-spec) documents every field.
