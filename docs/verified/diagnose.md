# `anyship diagnose` — verification runbook

A fill-in runbook for verifying `anyship diagnose` against the real Claude API.
No Claude credentials existed when this was written, so the API legs are
unfilled. Sections 1–3 are already verified and need no key; section 6 is the
part that waits, and OPE-29 is the card that runs it.

**Progress-table rule:** diagnose stays `done`. Do not promote it to `verified`
until section 6 is filled in from a real run.

Written against `19adc2d`, `go1.27.1`, `github.com/anthropics/anthropic-sdk-go
v1.78.0`.

---

## 1. What is already verified, with no key

`diagnose --show-context` prints the exact payload that would be sent to Claude
and then stops, without calling the API. Every section of that payload passes
through `Redactor.Redact` in `Context.add` (`diagnose/collect.go`), so the
payload's redaction coverage *is* the redactor's coverage, and it can be checked
for free.

Confirmed removed from the payload — in the raw spec, in the generated
`compose.yaml`, and in the user's own `--note`:

- the values of declared `spec.secrets` that are set in the environment
- `sk-ant-…`, `sk-…`, `ghp_…`, `github_pat_…`, `xox[abprs]-…`, `AKIA…`, JWTs
- PEM private key blocks
- `scheme://user:password@host` URLs
- secret-looking keys in any case (`API_KEY:`, `jwtsecret:`, `apiKey:`),
  `password=…` in a libpq string, `aws_session_token = …`, `x-api-key:` headers,
  `declare -x DB_PASSWORD="…"` shell env dumps

Correctly *not* touched: spec structure under secret-looking keys
(`secrets: [JWT_SECRET]`, `secrets:`, `JWT_SECRET: {generate: hex32}`), and
values that were already redacted.

### 1.1 Three known leaks — read before sending a real spec

These are open bugs, not fixed. If your spec contains any of these shapes, the
secret **will** be sent to Claude in clear text.

| Leak | Shape | Issue |
|---|---|---|
| URL credentials with an empty username | `redis://:pw@host`, `amqp://:pw@host` | OPE-20 |
| Unquoted value containing whitespace — only the first word is redacted | `TOKEN_ARGS: --api-token V`, and YAML block scalars (`SECRET: \|`) | OPE-21 |
| `Authorization` headers | `Authorization: Bearer …`, `Authorization: Basic …` | OPE-22 |

Reproduce any of them in a few seconds:

```sh
anyship diagnose -t vps --show-context | grep -n 'zzLEAK'
```

with a spec whose `env` holds canary values in those shapes. **Always run
`--show-context` and grep for your own secrets before the first real call.**

## 2. Model and API wiring (reviewed, not exercised)

Reviewed against the current Claude API surface; nothing needs changing:

- `DefaultModel = "claude-opus-5-5"` and the `claude-sonnet-5-5` entry in
  `defaultFallbackModels` are real model ids — both are present as
  `anthropic.ModelClaudeOpus5_5` / `ModelClaudeSonnet5_5` in the vendored
  anthropic-sdk-go v1.78.0.
- The server-side refusal fallback uses the current `"default"` form
  (`server-side-fallback-2026-07-01` + `Fallbacks.OfDefault`), so a safety
  decline is re-served inside the same call.
- The system prompt (instructions + the spec JSON Schema) carries
  `cache_control` and the volatile deployment context follows it, so the stable
  prefix caches across the retry rounds.
- `MaxTokens: 16000` suits a non-streaming request; `--effort` accepts
  `low|medium|high|xhigh|max`, matching the API.

## 3. Minimum credentials

Only one credential, and it is not a cloud provider:

- **An Anthropic API key with Messages API access** — `ANTHROPIC_API_KEY`, or
  an `ant auth login` profile, or `ANTHROPIC_AUTH_TOKEN`.
- No Admin key. No organisation scopes. No cloud IAM of any kind.

Keep it least-privilege:

- Issue the key in a **dedicated workspace** with a **spend limit** (a few
  dollars is plenty — see section 4), so a runaway retry loop cannot cost more
  than the limit.
- Delete the key after the run.
- Check the organisation's **data-retention setting** before sending a real
  spec; the payload includes your `anyship.yaml` and your logs.

The target legs need whatever that target already needs — for the `vps` target,
a Linux host with Docker reachable over ssh, the same thing
`scripts/e2e-vps.sh` needs.

## 4. Cost estimate, and how to keep it down

Measured from a real `--show-context` payload:

| Part | Size | Approx. tokens |
|---|---|---|
| System prompt (instructions + spec JSON Schema) | 8.5 KB | ~2.5 K, cached |
| Deployment context, small app, failing ssh | 3.5 KB | ~1 K |
| Deployment context, realistic failure with logs | — | ~5–15 K |
| Worst case (all 7 sections at the 24 KB `maxSection` cap) | ~170 KB | ~45 K |

Output is capped at `MaxTokens: 16000`; at the default `--effort high` expect a
few thousand output tokens including thinking.

So a single realistic call is on the order of **15 K input + 4 K output**. At
Opus-tier pricing ($5 / $25 per MTok — *assumption, confirm `claude-opus-5-5`'s
own rate before quoting this*) that is roughly **$0.20 a run**, and
`MaxAttempts = 3` rounds of patch rejection put the worst case near **$0.50**.
A handful of runs is a couple of dollars.

To spend less:

- `--show-context` first, every time. It is free, and it is also the redaction
  check from section 1.
- `--effort low` or `medium` while you are still shaking out the plumbing; save
  `high` for the run you intend to record.
- `--no-checks` skips the target's dry run, which both shortens the payload and
  avoids touching the host.
- Keep the failing app small. Logs dominate the payload.
- The system prompt is cached, so consecutive runs within the cache TTL pay
  much less on input. Run your attempts back to back.

## 5. Data-residue checklist

`diagnose` creates nothing on a cloud provider, so there are no leftover
resources to delete. The residue is **the payload you sent**, and it is the
thing to account for:

- [ ] Ran `--show-context` and grepped the payload for every real secret value
      in the spec and in the environment
- [ ] Checked the three known leak shapes from section 1.1 against this spec
- [ ] Confirmed the organisation's data-retention setting before sending
- [ ] If anything leaked: **rotate that secret**, and note it in section 6
- [ ] Deleted the API key used for the run
- [ ] Nothing was written to `anyship.yaml` without intent — `diagnose` prompts
      before writing, and `--yes` skips that prompt. Run it inside a clean git
      worktree so an unwanted patch is one `git checkout` away.
- [ ] Tore down whatever the deliberately-failing deployment created on the
      target (for `vps`: `anyship destroy -t vps`, then confirm no leftover
      containers, volumes or `~/anyship-*` directories on the host)

## 6. The run — to be filled in

Build the binary and set the key:

```sh
go build -o ./anyship ./cmd/anyship
export ANTHROPIC_API_KEY=...
```

### 6.1 Break a deployment on purpose

Pick one failure and record which:

- a port that is already taken on the host
- a `start` command that exits immediately
- an image tag that does not exist
- a service that depends on a resource the spec never declares

```
Failure used: ____________________
Spec:         ____________________
```

### 6.2 Check the payload before paying for anything

```sh
./anyship diagnose -t <target> --show-context | tee ctx.md
grep -n -i -E '<your real secret values>' ctx.md   # must print nothing
```

```
Leaked anything?  yes / no
If yes, which:    ____________________   (open an issue, rotate the secret)
```

### 6.3 Ask Claude

```sh
./anyship diagnose -t <target> --note '<the symptom, in your words>' --json | tee diag.json
```

Expected shape — `diagnosis.summary`, `diagnosis.root_cause`,
`diagnosis.confidence`, `diagnosis.evidence[]`, `diagnosis.steps[]`,
`valid_patch`, and either `patched_spec` or `patch_problem`.

Record:

```
diagnosis.summary:      ____________________
diagnosis.confidence:   high / medium / low
Evidence quotes real log lines or finding codes?   yes / no
valid_patch:            true / false
patch_problem:          ____________________
Rounds needed (1–3):    ____     (a rejected patch is re-sent; MaxAttempts = 3)
Wall-clock:             ____
```

Failure modes worth recognising, all from `explainAPIError`:

| Output | Meaning |
|---|---|
| `the Claude API rejected the credentials` | 401/403 — key not set or wrong |
| `the Claude API doesn't know this model` | 404 — `--model` is wrong, or the default id has been retired |
| `the Claude API is rate limiting this key` | 429 — wait and retry |
| `the model declined to answer (<category>)` | refusal that the fallback did not cover |
| `the model's answer was cut off` | hit `MaxTokens`; retry at a lower `--effort` |
| `Claude proposed a change … but it didn't validate after 3 attempts` | the patch never passed `validatePatched`; paste `patch_problem` below |

### 6.4 Tokens and cost

**anyship does not report this today.** `Claude.Diagnose` drops `resp.Usage`,
and `diagnose --json` has no `usage` field (OPE-27). Until that lands, read the
numbers from the Anthropic Console's usage page for the run's time window:

```
Input tokens:        ____
Cache read tokens:   ____
Output tokens:       ____
Cost:                $____
Model actually used: ____   (a fallback may have served it)
```

### 6.5 Apply the patch and confirm the deployment comes up

```sh
./anyship diagnose -t <target> --note '<symptom>'   # answer y at the prompt
git diff anyship.yaml
./anyship plan -t <target>
./anyship apply -t <target>
```

```
Patch was sensible?          yes / no
Deployment succeeded after?  yes / no
If no, what still failed:    ____________________
```

Then work through section 5.

### 6.6 Outcome

```
Date:                 ____
Ran by:               ____
Verdict:              diagnose works / diagnose needs work
Issues opened:        ____________________
```

Promote diagnose to `verified` in the progress table only once this section is
filled in, and say there that it was verified by a real Claude API run on this
date.
