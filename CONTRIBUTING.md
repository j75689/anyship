# Contributing to anyship

Thanks for helping! Issues and pull requests are welcome.

## Setup

```bash
npm install
npm run check        # typecheck + tests
npm run anyship -- --help
```

Node.js 22+ is required. Packages export TypeScript sources directly and run through `tsx`/`vitest`,
so there is no build step during development.

## Writing an adapter

An adapter implements `Adapter` from `@anyship/core`:

```ts
interface Adapter {
  name: string;
  description: string;
  plan(spec: DeploySpec, ctx: AdapterContext): Promise<Plan>;
  apply(plan: Plan, spec: DeploySpec, ctx: AdapterContext): Promise<ApplyResult>;
}
```

Rules every adapter follows (see `packages/adapter-cloudflare` for a reference):

1. **`plan()` has no side effects.** It reads the spec and returns findings, actions and generated
   files. Nothing is written or deployed.
2. **Never silently drop a need.** If the platform can't provide something the spec asks for (a
   volume, a TCP port, a resource type), emit an `error` finding with a stable `code` (prefix it, e.g.
   `FLY_`) and a `hint` that suggests an alternative.
3. **`apply()` refuses a plan with errors**, and runs platform tools through `ctx.exec` so tests can
   stub them.
4. **Validate your `targets.<name>` block** with a strict schema so typos surface as findings.
5. **Rendering is deterministic.** The same spec always produces the same config. Don't embed
   timestamps or today's date.
6. **Credentials stay local.** Read them from the platform's own CLI login or environment variables;
   never send them anywhere else.

Add tests covering rendered config, every refusal code, and `apply()` with a stubbed `exec`. Then
register the adapter in `packages/cli/src/program.ts`.

## Changing the spec

The spec is the contract between detection and every adapter, so changes need care:

- Prefer adding optional fields over changing existing ones.
- Describe *needs*, not platform features (`volumes`, not `ebs`).
- Run `npm run schema` and commit the regenerated `schema/anyship.schema.json`.

## Commit style

Short imperative subject lines, e.g. `cloudflare: support queue consumers`.
