# The `kubernetes` target

Deploys to any Kubernetes cluster through your `kubectl`, as a Deployment and a Service per spec
service, in a namespace you name.

```yaml
spec:
  targets:
    kubernetes:
      namespace: my-app                   # existing namespace the spec is deployed to
      context: my-cluster                 # optional; kubeconfig context, the current one when empty
      repository: ghcr.io/me/my-app       # registry path to push images built from source to
      platform: linux/arm64               # optional; what to build images for, the nodes' architecture when empty
      ingressClass: nginx                 # optional; the IngressClass public HTTP ports get an Ingress with
      cronImage: curlimages/curl:8.14.1   # optional; what a cron entry with a path calls it from
      storageClass: fast                  # optional; the StorageClass volumes are claimed from, the default when empty
      services:                           # optional; settings of single services
        web:
          serviceAccount: web             # existing ServiceAccount the pods run as
          maxReplicas: 10                 # autoscale between replicas and this on CPU use
          resources:                      # override what memory and cpu put in each slot
            limits:
              cpu: none                   # no CPU limit; "0.5", "2", "1GB" or none per slot
```

- Each `server` becomes a Deployment and a ClusterIP Service named `<spec name>-<service>`, labeled
  `anyship-project=<spec name>`. The objects are rendered to `.anyship/kubernetes/manifests.yaml`
  for review and sent with `kubectl apply --prune`: a service taken out of the spec loses its
  Deployment and Service on the next `apply`, and `status`, `logs` and `destroy` find everything by
  the label. anyship keeps no state and creates no namespace: preflight checks that the context and
  the namespace exist and that your login may create Deployments there, and names the command that
  creates a missing namespace.
- `apply` waits for each Deployment to roll out, 5 minutes at most. A revision whose pods don't
  become ready fails the apply and leaves the previous pods serving; the result names the pods and
  the cluster's warnings about them (an image that can't be pulled, a container that keeps
  crashing), and `status` shows the pod's state (`ImagePullBackOff`, `CrashLoopBackOff`) with its
  message.
- Services without `image` or `dockerfile` are built from a generated Dockerfile
  (`.anyship/kubernetes/<service>.Dockerfile`) with `docker buildx build --push`, pushed to
  `<repository>/<spec name>-<service>` and deployed by digest. The registry has to be one the
  cluster's nodes can pull from; a local cluster that shares the Docker daemon (OrbStack, Docker
  Desktop) can use a `registry:2` container at `localhost:5000`. Images are built for the nodes'
  architecture, read from the cluster; `platform` overrides that, and a cluster with mixed nodes
  gets `linux/amd64` unless it does.
- A service that sets `image` runs that image. If the tag can move (`:latest`, `:main`, no tag),
  every `apply` rolls the pods and pulls it again (`imagePullPolicy: Always`), so they run what the
  tag points at then; `plan` notes it (`K8S_MUTABLE_TAG`). A versioned tag or a digest rolls out only
  when the spec changes, and `--image <service>=<image>` deploys another image for one run.
- One HTTP port per service (default 8080, also passed as `$PORT`), served by the Service, plus any
  number of TCP and UDP ports. `${services.<name>.url}` in `env` becomes
  `http://<spec name>-<name>.<namespace>.svc:<port>`, the HTTP port, which follows from the names, so
  it is known before the first deploy; a service with ports but no HTTP one has no address
  (`K8S_SERVICE_URL`). An `internal` port is reachable inside the cluster only.
- A `public` HTTP port gets an Ingress when the spec names an `ingressClass` (`kubectl get
  ingressclass` lists the cluster's; preflight checks it exists): `domains` are its host rules, and
  without them it matches any host. anyship issues no certificate and makes no DNS record; TLS is
  the ingress controller's to add. Without `ingressClass` no Ingress is made and the port is reachable
  inside the cluster only, which `plan` says (`K8S_PUBLIC_PORT`); `domains` then are refused
  (`K8S_DOMAINS_NEED_INGRESS`).
- A `public` TCP or UDP port makes the service's Service one of type LoadBalancer (`tcp+udp` is two
  ports of it): the cluster's load balancer gives it an address, which `apply` prints once assigned
  and `anyship status` shows. A cluster without a load balancer implementation leaves it pending.
- `replicas` is the Deployment's replica count. `memory` and `cpu` become the container's resource
  requests and limits, both, so the pods get what the spec says and no more. A CPU limit throttles
  the container when it reaches it, even while the node has idle CPU, which raises latency at peaks
  and shows up nowhere in `status`; `services.<name>.resources` overrides any of the four slots
  (`requests`/`limits` × `cpu`/`memory`) in the spec's formats, and `none` leaves a slot unset, so
  `limits.cpu: none` is how to run with CPU requests only. A limit below its request, or a limit
  for a resource with no request, is refused (`K8S_RESOURCES`). The spec decides: removing an
  override puts the spec's value back on the next `apply`.
- `services.<name>.maxReplicas` adds a HorizontalPodAutoscaler that keeps between `replicas` and
  that many pods, adding one when average CPU use passes 80% of the request; it needs `cpu` (or a
  cpu request override) and the cluster's metrics API, which preflight checks
  (`K8S_PREFLIGHT_METRICS`; install metrics-server). The Deployment then carries no replica count,
  so an `apply` doesn't put the pods back to the minimum; the first `apply` after adding the
  option may drop them to one for a moment until the autoscaler takes over. A service with volumes
  can't autoscale.
- `services.<name>.serviceAccount` names the existing ServiceAccount the pods (and the cron
  command jobs) run as; preflight checks it exists in the namespace
  (`K8S_PREFLIGHT_SERVICE_ACCOUNT`). Bind its roles yourself.
- `start` runs as the container's arguments (`args`), exactly as written and without a shell, with
  the image's own entrypoint; wrap it in `sh -c '...'` if you need pipes or variables.
- `healthCheck.path` becomes the startup and readiness probes: a new pod gets traffic once `GET`
  on the path answers with a 2xx or 3xx status, the rollout fails if it hasn't within 4 minutes, and
  a pod that stops answering three times in a row is taken out of the Service. `healthCheck.command`
  runs in the container through `sh -c` instead. The spec decides: a deploy without `healthCheck`
  removes the probes.
- Secrets are Secret objects named `<spec name>-<secret>` in lowercase, labeled with the project,
  and reach the container as files at `/run/secrets/<NAME>`. `generate: "hex32"` secrets are created
  on first deploy and kept; others come from the same-named environment variable at `apply` time, or
  stay as set by a previous deploy. A changed value rolls the pods of the services that use it.
  `destroy --volumes` deletes the secrets; plain `destroy` keeps them, and the images stay in the
  registry.
- A `static` site is built into an nginx image from a generated Dockerfile (`build.command` runs
  first for a JavaScript project) and served on port 80 behind its Service, on the spec's port or
  80. A `worker` is a Deployment with no Service: no port, no `$PORT`, no address for
  `${services.<name>.url}`, and `healthCheck.command` as its only probe.
- `volumes` become PersistentVolumeClaims named `<spec name>-<service>-<volume>`, ReadWriteOnce,
  of `size`, from `storageClass` or the cluster's default (preflight checks one exists and lists
  the cluster's classes when it doesn't); `class` isn't applied. One pod holds a volume at a time,
  so such a service runs one replica (`K8S_VOLUME_REPLICAS`) and a deploy stops the old pod before
  it starts the new one. The claims are never pruned: a volume taken out of the spec keeps its
  claim and its data, and `destroy --volumes` deletes them (plain `destroy` keeps them).
- A `cron` entry becomes a CronJob named `<spec name>-<service>-cron-<n>`, read in UTC, that runs
  one Job per schedule tick: an entry with a `path` runs curl from `cronImage` against
  `http://<spec name>-<service>.<namespace>.svc:<port><path>` with `method` (`POST` by default), and
  an entry with a `command` runs it in the service's image, split like `start`, with the service's
  env and secrets. A call gives up connecting after 10 seconds and waits 30 minutes at most for the
  response, so a pod gone mid-call fails the run instead of hanging it. Runs don't overlap (a tick
  while the last run is still going is skipped), a failed run is retried twice, and the last three
  successful and failed Jobs are kept for `kubectl get jobs`; their logs are part of `anyship logs`. The jobs' pods don't carry the service's label,
  so the Service never sends them traffic. An entry taken out of the spec loses its CronJob on the
  next `apply`, and `destroy` removes them all.
- `logs` runs `kubectl logs` over every pod of the spec, prefixed with the pod's name, and follows
  with `-f`. `status` reads the Deployments and their pods.
- Refused with a reason: resources anyship would have to provision (`K8S_RESOURCE`; run the
  database yourself and mark it external).

[All targets](../../README.md#targets)
