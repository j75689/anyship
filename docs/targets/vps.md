# The `vps` target

Deploys to any Linux server with Docker, over your system `ssh`, as a Docker Compose project.

```yaml
spec:
  targets:
    vps:
      host: deploy@203.0.113.10        # ssh destination or ~/.ssh/config alias
      port: 22                         # optional
      identityFile: ~/.ssh/id_ed25519  # optional
      dir: anyship/my-app              # optional; relative to the login user's home
      sudo: false                      # run docker via `sudo -n`
```

- The spec becomes a Docker Compose project (`.anyship/vps/compose.yaml`, kept locally for review).
  `apply` uploads it with the build context of every service that builds from source over `ssh`,
  then runs `docker compose up -d --build` on the host. The host needs Docker with the Compose plugin.
- Services without `image` or `dockerfile` are built from a generated Dockerfile
  (`.anyship/vps/<service>.Dockerfile`). Static sites are served by nginx on their public ports, or
  on port 80.
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
- Every `apply` first runs preflight checks over ssh, and `apply --dry-run` stops after them:
  Docker Compose on the host must accept the generated `compose.yaml` (validated in a temporary
  directory that is removed afterwards), the host's architecture is reported (with a warning on
  arm64 for image-based services), free disk space is compared with the declared volume sizes, and
  published ports must be free unless the project is already running there. Nothing is uploaded or
  started until the checks pass.
- Not yet: cron, provisioning databases (declare them as services instead), `domains` and HTTPS
  (`VPS_DOMAIN_UNSUPPORTED`; put your own reverse proxy in front of the published ports for now), and
  generated Dockerfiles for languages other than JavaScript, Go, Python and Rust. `plan` explains
  each refusal.

[All targets](../../README.md#targets)
