# Verified: the MCP server from Claude Code

What was run, what worked, and where an agent gets stuck. Nothing here is from reading the code; every
quote is copied from a real response.

- **Date:** 2026-10-02
- **anyship:** `0.2.1-0.20261002062437-19adc2d9b46e` (built from `19adc2d`)
- **Client:** Claude Code 2.1.287, macOS 25.6.0 (arm64)
- **Verdict:** the server works. Setup is one command and needs no fixing. The problems are all in the
  *wording*: three stale hints send an agent to the wrong place, and two tools write files while
  claiming to be read-only.

## Setup

The README command works exactly as written:

```console
$ claude mcp add anyship -- anyship mcp
Added stdio MCP server anyship with command: anyship mcp to local config

$ claude mcp list
Checking MCP server health…
anyship: anyship mcp - ✔ Connected
```

`claude mcp get anyship` confirms the scope and that it stays connected:

```console
$ claude mcp get anyship
anyship:
  Scope: Local config (private to you in this project)
  Status: ✔ Connected
  Type: stdio
  Command: anyship
  Args: mcp
```

Two things the README does not say, both of which matter:

- **`anyship` must be on `PATH`** when Claude Code starts the server, not just in your shell. Claude
  Code runs the command itself.
- **The server's working directory is wherever Claude Code was started.** That directory is what
  `config` and `dir` default to. See [Finding 5](#finding-5-the-agent-cannot-tell-which-directory-the-server-is-in).

Claude Code picks up all nine tools and prefixes each with the server name:

```
mcp__anyship__targets      mcp__anyship__detect   mcp__anyship__validate
mcp__anyship__plan         mcp__anyship__status   mcp__anyship__logs
mcp__anyship__diagnose_context
mcp__anyship__apply        mcp__anyship__destroy
```

The server's `instructions` reach the client and state the flow, so the agent does not have to guess
the order:

> Typical flow: detect (draft a spec for a directory) → write anyship.yaml → validate → plan → apply
> with dry_run → apply.

## What was exercised

A throwaway Node app (`dockerfile/testdata/apps/node`) with no `anyship.yaml`, and a monorepo with a
FastAPI app at `apps/api`. The `vps` target pointed at an ssh host that refuses connections, so the
failure paths are real rather than mocked.

| Call | Result |
|---|---|
| `targets` | all four targets with capabilities |
| `detect` (Node app) | valid spec, `DETECT_PORT_ASSUMED` info finding |
| `detect` (monorepo root) | error finding, no spec — see Finding 5 |
| `detect` `dir=apps/api` | valid spec, framework and start command correct |
| `validate` before any spec exists | `valid: false`, raw `*PathError` — see Finding 4 |
| `validate` wrong `secrets` shape | Go reflection error — see Finding 3 |
| `validate` correct spec | `valid: true`, name and services |
| `plan` `vps` without `host` | `VPS_BAD_OPTIONS` + hint — see Finding 1 |
| `plan` `vps` ready | 2 actions, 2 info findings, 2 file paths — see Finding 2 |
| `plan` `cloudflare` | `CF_NO_ENTRY`, refuses the Node app and says why |
| `plan` `target=VPS` | `unknown target "VPS"; available: aws, cloudflare, gcp, vps` |
| `plan` with no `target` | schema rejection from the client, never reaches anyship |
| `status` `vps` unreachable | bare exit code, no cause — see Finding 6 |
| `status` `cloudflare` | `the cloudflare target does not report status yet` |
| `logs` unknown service | `unknown service "api"; services: web` |
| `logs` `since=yesterday` | rejected, with the accepted formats |
| `apply` without `--allow-deploy` | refused, names the flag |
| `apply` `dry_run=true` | ran checks, **and wrote two files** — see Finding 2 |
| `diagnose_context` | full redacted report, the best tool here |
| `destroy` `dry_run=true` | plain-language summary of what it would remove |
| `destroy` `volumes=true` no confirm | refused, names the expected value |
| `destroy` `volumes=true` wrong confirm | refused |

### The safety gates hold

Both work as the README promises, checked against a server started each way:

```
apply   {"target":"vps"}                      → this anyship MCP server only allows dry runs;
                                                restart it with `anyship mcp --allow-deploy` to deploy
destroy {"target":"vps","volumes":true}       → deleting data needs confirm_project set to "node-app",
                                                confirmed with the user
destroy {"target":"vps","volumes":true,
         "confirm_project":"wrong-name"}      → same refusal
```

### Secrets really are redacted

With `API_TOKEN=sk-live-…` exported and declared in the spec, `diagnose_context` returned the secret's
*name* in seven places and its *value* in none. Grepping the whole response for the value found nothing.

## Findings

Each one is a usability problem for an agent, not a crash. Rewriting the tool schemas is out of scope
for this card; these want their own issues.

### Finding 1: three adapters hint JSON into a YAML file, at two different nesting depths

The spec became a Kubernetes-style YAML manifest in `24ab1ec`, but the hints did not follow.

```
plan {"target":"vps"} →
  "message": "targets.vps: host is required",
  "hint": "Set targets.vps.host to an ssh destination, e.g. {\"vps\": {\"host\": \"deploy@203.0.113.10\"}}."
```

The hint is wrong twice: the field lives at `spec.targets.vps.host`, not `targets.vps.host`, and the
example is JSON for a file the agent is writing as YAML. Following it literally — which is the obvious
thing for an agent to do — produces a broken spec:

```yaml
targets: {"vps": {"host": "deploy@203.0.113.10"}}
```

```
validate → "problems": ["not a valid spec: json: unknown field \"targets\""]
```

All three offenders disagree with each other:

| Where | Hint | Problem |
|---|---|---|
| `adapters/vps/vps.go:100` | `Set targets.vps.host … {"vps": {"host": …}}` | path misses `spec.`, example is JSON |
| `adapters/gcp/gcp.go:121` | `Set targets.gcp … {"gcp": {"project": …}}` | path misses `spec.`, example is JSON |
| `adapters/aws/aws.go:158` | `Set spec.targets.aws … {"region": …}` | path right, but the example is the *inner* object |

The correct shape, for reference:

```yaml
spec:
  targets:
    vps:
      host: deploy@203.0.113.10
```

### Finding 2: `plan` points at generated files that do not exist, and `dry_run` writes them anyway

`plan` returns a finding that tells the agent to go and read something:

```
"code": "VPS_GENERATED_DOCKERFILE",
"message": "No Dockerfile, so anyship generated one; review it in the generated files below.",
```

and the paths to read:

```
"files": ["/…/sandbox/.anyship/vps/compose.yaml", "/…/sandbox/.anyship/vps/web.Dockerfile"]
```

Neither file exists. `plan` computes them and writes nothing — correct for a read-only tool, but the
agent was just told to review them, so it reads the paths and gets *file not found*. There is no tool
that returns generated file *contents*, so an agent cannot honour its own instruction except by
calling `apply`.

Which leads to the other half. Here is which calls actually touch the disk, each measured from a clean
directory:

| Call | Annotation | Files written under `.anyship/` |
|---|---|---|
| `targets`, `detect`, `validate`, `plan`, `status` | read-only | 0 |
| `destroy` `dry_run=true` | destructive | 0 |
| **`apply` `dry_run=true`** | destructive | **2** |
| **`diagnose_context`** | **read-only** | **2** |

```
apply {"target":"vps","dry_run":true} →
  "output": "wrote .anyship/vps/compose.yaml\nwrote .anyship/vps/web.Dockerfile\n$ ssh …"
```

`diagnose_context` carries `readOnlyHint: true`, so a host may run it without asking, and it writes
into the user's project. `apply` with `dry_run` is described as "only run the target's checks", which
does not prepare the agent for two new files either. The README's "Changes anything: no" column is
wrong for `diagnose_context`.

### Finding 3: shape errors are raw Go, and the schema an agent would consult is a 404

Guessing `secrets` as a list instead of a map produced:

```
anyship.yaml: not a valid spec: json: cannot unmarshal array into Go struct field
Manifest.spec.secrets of type map[string]*spec.Secret
```

This leaks Go type names (`Manifest.spec.secrets`, `map[string]*spec.Secret`), says `json:` about a
YAML file, and gives no line number and no example. An agent *can* recover — "it wants a map" is
readable enough — but the natural next move is to fetch the schema named at the top of every spec
`detect` generates, and that fails:

```console
$ curl -o /dev/null -w '%{http_code}' https://raw.githubusercontent.com/j75689/anyship/main/schema/anyship.schema.json
404
```

The repo is private, so the `$schema` URL resolves for nobody. The server exposes no resource or tool
that returns the schema, so a stuck agent has no way back other than guessing again.

### Finding 4: errors name `anyship.yaml` without saying where it looked

```
validate {} → "problems": ["open anyship.yaml: no such file or directory"]
plan {"target":"vps"} → anyship.yaml: open anyship.yaml: no such file or directory
```

A bare relative path. The agent does not know the server's working directory (Finding 5), so it cannot
tell whether the file is missing or whether it is looking in the wrong place. Neither message mentions
`detect`, even though the server instructions name it as step one.

### Finding 5: the agent cannot tell which directory the server is in

Every `config` and `dir` description says "defaults to … the server's working directory", but no tool
reports what that directory is, and the agent's own working directory is not necessarily the same one.
In the monorepo case this is the difference between working and not:

```
detect {} →
  "findings": [{"code": "DETECT_UNKNOWN",
                "message": "Could not work out how to build or start this project.",
                "hint": "Add a Dockerfile, or fill in services.web in anyship.yaml. …"}]

detect {"dir": "apps/api"} →
  "evidence": ["requirements.txt found → Python project", "FastAPI app \"app\" in main.py", …]
  valid: true
```

The hint on the failure is actively misleading at a repo root: the fix is not to add a Dockerfile, it
is to pass `dir`. `DETECT_UNKNOWN` should mention `dir` when the directory holds no recognisable
project but does hold subdirectories.

### Finding 6: `status` throws away the cause that every other tool keeps

Same unreachable host, three tools:

```
status {"target":"vps"}
  → reading status from deploy@127.0.0.1 failed (exit status 255)

logs {"target":"vps","tail":20}
  → reading logs from deploy@127.0.0.1 failed (exit status 255);
    has node-app been deployed there with `anyship apply -t vps`?

apply {"target":"vps","dry_run":true}
  → "Cannot run the preflight checks on deploy@127.0.0.1 (exit status 255).
     Check that `ssh deploy@127.0.0.1` works without a password prompt."
    output: "ssh: connect to host 127.0.0.1 port 22: Connection refused"
```

`status` gives an agent nothing to act on. The ssh stderr exists — `diagnose_context` prints
`Connection refused` in its own Status section — so `status` is discarding information it already has.

### Finding 7: error messages tell an MCP agent to run CLI commands

`logs` suggests `anyship apply -t vps` and `apply` suggests checking `ssh deploy@…`. An agent reached
the server over MCP and may have no shell at all. These should name the tool (`apply` with
`dry_run`) and let the agent relay the shell advice to the user, rather than assume a terminal.

### Finding 8: `idempotentHint: false` on tools that are plainly idempotent

`targets`, `detect`, `validate` and `plan` all ship `"idempotentHint": false` alongside
`"readOnlyHint": true`. Nothing sets it, so the Go SDK serialises the zero value. Calling `targets`
twice is as idempotent as anything gets. Cosmetic, but it is wrong in every schema a host reads.

## What did not get tested, and why

- **No natural-language round in an interactive Claude Code session.** The nested `claude -p` run
  reached the server — the init event lists all nine `mcp__anyship__*` tools — and then stopped at
  `Not logged in · Please run /login`, because this agent's environment has no credentials a child
  session can use. Everything above was therefore driven over raw stdio JSON-RPC against the same
  binary Claude Code connects to, with the tool schemas and `instructions` exactly as the client
  received them. What that misses is host behaviour around the destructive hints: whether Claude Code
  prompts before `apply` and `destroy`, and how it renders the refusals.
- **No successful deploy**, so `logs` with a live stream, the 20s window `note`, and `truncated`
  output were never seen in a passing state. `vps` deploys are covered by the `vps-e2e` CI job.
