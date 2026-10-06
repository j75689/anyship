# Security policy

## Supported versions

anyship is pre-1.0. Security fixes land on `main` and ship in the next release; only the latest
release is supported.

## Reporting a vulnerability

Please don't open a public issue for a security problem. Report it privately through GitHub:
**Security → Report a vulnerability** on this repository, or
<https://github.com/j75689/anyship/security/advisories/new>.

Include the version (`anyship --version`), the target, the steps to reproduce, and what an attacker
gains. This is a small project, so expect a first reply within a week. You will be credited in the
advisory unless you ask not to be.

## What counts

anyship runs on your machine and drives `ssh`, `docker`, `wrangler`, `gcloud` and `aws` with your
own logins. It has no server and stores no credentials. Reports that matter most:

- A spec, a project directory or platform output that makes anyship run a command, or write a file,
  that the plan did not show.
- Secrets reaching plan output, logs, generated files under `.anyship/`, or the context `diagnose`
  and the MCP `diagnose_context` tool send, past the redaction.
- The MCP server deploying or destroying without `--allow-deploy`, or deleting data without
  `confirm_project`.
- `install.sh` or a release archive installing something that doesn't match `checksums.txt`.

Out of scope: vulnerabilities in the platforms and CLIs anyship drives (report those upstream), and
a spec that deploys exactly what it says.
