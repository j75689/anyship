# Contributing to anyship

Thanks for helping! Issues and pull requests are welcome. Everyone taking part is expected to follow
the [code of conduct](CODE_OF_CONDUCT.md). Security problems go through [SECURITY.md](SECURITY.md),
not a public issue.

## Setup

Go 1.27+ is required. CI and releases build with the version in `go.mod`, so formatting and
vetting with the same one avoids differences between Go releases.

```bash
make build    # ./anyship
make test     # unit tests with the race detector
make check    # everything CI checks on Linux: gofmt, vet, go mod tidy, lint, tests, build
make help     # the other targets
```

`make check` needs [golangci-lint](https://golangci-lint.run/docs/welcome/install/) v2, built with
the Go version you run; CI lints every pull request either way, so `make test` is enough to start.

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
- Regenerate the JSON Schema with `make schema`. A test fails if you forget.

## Releasing

A pull request that changes what users see adds a line under `## [Unreleased]` at the top of
[CHANGELOG.md](CHANGELOG.md), creating the section if the last release closed it. The release notes
are that section, so it is finished first, in a pull request of its own:

1. Rename `## [Unreleased]` to the release: `## [0.3.0] - 2026-10-06`, with a `⚠ BREAKING CHANGES`
   block for anything that breaks an existing spec and the steps to move across. `make release-notes
   VERSION=0.3.0` prints what will be published.
2. Update the links at the bottom of the file: the new version compares the previous tag with its
   own.
3. After the pull request is merged, tag that commit on `main` and push the tag:

```bash
git tag -a v0.3.0 -m "v0.3.0"
git push origin v0.3.0
```

The `Release` workflow refuses a tag that is not on `main` or has no dated changelog section, runs
the tests, builds the archives for every OS and CPU with `make dist`, which also writes
`checksums.txt`, and publishes the GitHub release with the changelog section as its notes. Tags with a suffix such as
`v1.0.0-rc.1` become pre-releases. Release tags can't be moved or deleted once pushed, so check the
commit before you push one. Every pull request already builds the same archives with `make dist`,
installs one with `install.sh` and reads the latest changelog section, so a broken release config
fails CI before anything is tagged.

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
