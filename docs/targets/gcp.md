# The `gcp` target

Deploys HTTP services to Google Cloud Run with your installed `gcloud`.

```yaml
spec:
  targets:
    gcp:
      project: my-project   # Google Cloud project id
      region: us-central1
      repository: apps      # existing Artifact Registry Docker repository; needed to build from source
      private: false        # true: public services require authentication
      configuration: work   # optional gcloud configuration (account + project); the active one by default
      serviceAccount: runner@my-project.iam.gserviceaccount.com # optional; default: Compute Engine default account
      timeout: 10m          # optional request timeout, up to 1h; default: keep the service's (5m when new)
```

- anyship drives your installed `gcloud` with its current login, and `docker buildx` for builds. It
  creates nothing outside Cloud Run and Secret Manager: create the Artifact Registry repository once
  (`gcloud artifacts repositories create apps --repository-format docker --location us-central1`).
- Each service becomes the Cloud Run service `<spec name>-<service>`, labeled
  `anyship-project=<spec name>`; `status`, `logs` and `destroy` find it by that label, with no local state.
- Services that build from source are built for `linux/amd64`, pushed, and deployed by digest, so a
  deploy runs exactly the image it built. Generated Dockerfiles are kept in `.anyship/gcp/` for review.
- A service that sets `image` is deployed from exactly that reference. A tag that moves (`:latest`,
  `:main`, no tag at all) is pulled again on every deploy, and behind a caching registry it can
  resolve to an older image while the deploy reports success, so `plan` warns about it
  (`GCP_MUTABLE_TAG`). Pin the image by digest, or pass the one to deploy with
  `--image <service>=<image>`.
- Cloud Run pulls images only from Artifact Registry, `gcr.io` and Docker Hub, so `plan` refuses an
  `image` on any other registry (`GCP_UNPULLABLE_IMAGE`). To deploy an image from `ghcr.io` or
  another registry, pull it through an Artifact Registry
  [remote repository](https://cloud.google.com/artifact-registry/docs/repositories/remote-repo)
  and name it by that path, or push it to Artifact Registry.
- Every `apply` first checks the login, the project, the required APIs (`run`, `artifactregistry`,
  `secretmanager`) and the repository; `apply --dry-run` stops after the checks. Errors name the
  gcloud account in use, which matters when you have several (`gcloud auth list`).
- Secrets live in Secret Manager as `<spec name>-<NAME>` and reach the container as environment
  variables. A value in the deployer's environment adds a new version; `generate: "hex32"` secrets
  are created once and kept. Each apply lets the account the services run as read those secrets, and
  only those (`roles/secretmanager.secretAccessor` on each secret). `destroy --volumes` deletes them;
  images stay in the registry.
- One HTTP port per service (default 8080, also passed as `$PORT`). `replicas` sets the minimum
  instance count. Services reach each other by URL, not by name.
- `memory` and `cpu` set the instance size. Cloud Run takes 1, 2, 4, 6 or 8 CPUs and ties memory to
  them (up to 4GB with 1 CPU, 8GB with 2, 2GB to 16GB with 4, 4GB to 24GB with 6, 4GB to 32GB with
  8); `plan` refuses a pair that doesn't fit, and a fraction of a CPU, which Cloud Run only runs
  with one request per instance. The spec decides: a service that names neither is deployed with
  Cloud Run's defaults, 512MB and 1 CPU, so a size set by hand is replaced on the next deploy.
- A service with an `internal` port is deployed with internal ingress and requires authentication.
  anyship stops there, so as deployed the spec's other services can't call it, and `plan` warns
  about it (`GCP_INTERNAL_CALLERS`). Each caller needs three things set up by hand: a
  [route through a VPC network](https://cloud.google.com/run/docs/securing/private-networking),
  `roles/run.invoker` on the service for the account it runs as, and code that
  [attaches an identity token](https://cloud.google.com/run/docs/authenticating/service-to-service).
  The alternative is a public port and a token checked by the app.
- `healthCheck.path` becomes the service's startup probe: a new revision gets traffic only once that
  path answers with a 2xx or 3xx status, and a deploy whose revision doesn't answer within 4 minutes
  fails and leaves the previous revision serving. The probe sends no credentials, so the path must
  answer without them. `healthCheck.command` isn't applied
  (`GCP_HEALTH_COMMAND_IGNORED`); Cloud Run has no command checks. The spec
  decides: a deploy without `healthCheck.path` removes the service's startup probe, one set by hand
  included.
- `timeout` applies to every service of the spec. Without it, a deploy leaves the timeout as it is,
  so one set by hand survives; set it in the spec to keep it in one place.
- Refused with a reason: static sites, workers, volumes, cron, TCP/UDP ports, `domains`
  (`GCP_DOMAIN_UNSUPPORTED`; map them with `gcloud beta run domain-mappings` or a load balancer) and
  resources anyship would have to provision. `logs -f` points to `gcloud beta run services logs tail`.

[All targets](../../README.md#targets)
