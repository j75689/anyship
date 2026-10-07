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
```

- Each service becomes a Deployment and a ClusterIP Service named `<spec name>-<service>`, labeled
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
  requests and limits, both, so the pods get what the spec says and no more.
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
- `logs` runs `kubectl logs` over every pod of the spec, prefixed with the pod's name, and follows
  with `-f`. `status` reads the Deployments and their pods.
- Not yet, each refused with a reason: `cron`
  ([#100](https://github.com/j75689/anyship/issues/100)), volumes, static sites and workers
  ([#101](https://github.com/j75689/anyship/issues/101)), a replica ceiling and a service account
  ([#102](https://github.com/j75689/anyship/issues/102)), and resources anyship would have to
  provision.

[All targets](../../README.md#targets)
