# Contributing to anyship

Thanks for helping! Issues and pull requests are welcome.

## Setup

Go 1.26+ is required.

```bash
go test ./...
go run ./cmd/anyship --help
golangci-lint run ./...   # optional locally; CI runs it
```

## Writing an adapter

An adapter implements `adapter.Adapter`:

```go
type Adapter interface {
	Name() string
	Description() string
	Plan(ctx context.Context, s *spec.Spec, env *adapter.Env) (*adapter.Plan, error)
	Apply(ctx context.Context, p *adapter.Plan, s *spec.Spec, env *adapter.Env) (*adapter.Result, error)
}
```

Rules every adapter follows (see `adapters/cloudflare` for a reference):

1. **`Plan` has no side effects.** It reads the spec and returns findings, actions and generated
   files. Nothing is written or deployed.
2. **Never silently drop a need.** If the platform can't provide something the spec asks for (a
   volume, a TCP port, a resource type), emit an `adapter.Error` finding with a stable `Code` (prefix
   it, e.g. `FLY_`) and a `Hint` that suggests an alternative. Return a Go `error` only for real
   failures, not for unmet needs.
3. **`Apply` refuses a plan with errors**, and runs platform tools through `env.Exec` so tests can
   stub them.
4. **Decode your `targets.<name>` block strictly** (`DisallowUnknownFields`) so typos surface as
   findings.
5. **Rendering is deterministic.** The same spec always produces the same config: iterate maps in
   sorted order and don't embed timestamps or today's date.
6. **Credentials stay local.** Read them from the platform's own CLI login or environment variables;
   never send them anywhere else.
7. **Keep no state.** Never record what was deployed, and never read `env.OutDir` back to find out:
   ask the platform. Address deployments from the spec alone (names, hosts, ids in `targets.<name>`),
   and make `Apply` and `Destroy` safe to repeat. If something must be created, find it by name on
   the platform first instead of remembering that it was made.

Add tests covering rendered config, every refusal code, and `Apply` with a stubbed `Exec`. Then
register the adapter in `internal/cli/cli.go`.

## Changing the spec

The spec is the contract between detection and every adapter, so changes need care:

- Prefer adding optional fields over changing existing ones.
- Describe *needs*, not platform features (`volumes`, not `ebs`).
- Regenerate the JSON Schema: `go run ./cmd/anyship schema > schema/anyship.schema.json`. A test fails
  if you forget.

## Releasing

Before tagging, give the release its section in [CHANGELOG.md](CHANGELOG.md): the version, the date,
and a `⚠ BREAKING CHANGES` block for anything that breaks an existing spec, with a link to the
migration steps.

Releases are cut by pushing a version tag on `main`:

```bash
git tag -a v0.2.0 -m "v0.2.0"
git push origin v0.2.0
```

The `Release` workflow runs the tests, then GoReleaser builds the archives for every OS and CPU,
writes `checksums.txt`, groups the changelog by commit type, and publishes the GitHub release. Tags
with a suffix such as `v1.0.0-rc.1` become pre-releases. Every pull request already builds the same
archives in snapshot mode and installs one with `install.sh`, so a broken release config fails CI
before anything is tagged.

## Commit style

Commits follow [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/):

```
<type>[optional scope]: <description>

[optional body]

[optional footer(s)]
```

- **Types:** `feat` (new feature), `fix` (bug fix), plus `docs`, `test`, `refactor`, `perf`, `build`,
  `ci`, `chore` and `style`.
- **Scopes** name the area touched: `spec`, `detect`, `cli`, `adapter`, `cloudflare` (or another
  adapter's name), `ci`.
- **Breaking changes** (for example, an incompatible change to `anyship.yaml`) add `!` after the
  type/scope and a `BREAKING CHANGE:` footer explaining the migration.

Examples:

```
feat(cloudflare): support queue consumers
fix(cli): refuse to deploy when stdin is not a terminal
docs: explain how to write an adapter
feat(spec)!: rename services.<name>.uses to bindings

BREAKING CHANGE: rename "uses" to "bindings" in anyship.yaml.
```
