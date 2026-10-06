# The `aws` target

Deploys HTTP services to Amazon ECS Express Mode with your installed `aws` CLI.

> **Not verified yet.** This target has been tested against a fake `aws` CLI, never a real account.
> Express Mode provisions a load balancer that bills whether or not it serves traffic, so read the
> runbook and residual-resource checklist in [#26](https://github.com/j75689/anyship/issues/26)
> before you point it at an account you pay for.

```yaml
spec:
  targets:
    aws:
      region: us-east-1
      profile: work                 # optional aws CLI profile
      cluster: default              # existing ECS cluster
      repository: apps              # existing ECR repository; needed to build from source
      executionRole: ecsTaskExecutionRole                       # name or ARN (default shown)
      infrastructureRole: ecsInfrastructureRoleForExpressServices # name or ARN (default shown)
      taskRole: my-app-role         # optional, for the app's own AWS access
      subnets: [subnet-0abc]        # optional; default: the default VPC's public subnets
      securityGroups: [sg-0abc]     # optional, with subnets
      cpu: "1024"                   # optional, per task
      memory: "2048"
      maxTasks: 4                   # autoscaling ceiling (at least replicas)
```

- anyship drives your installed `aws` CLI (v2, with ECS Express Mode support) with its current login,
  and `docker buildx` for builds. App Runner no longer takes new customers, so services run on
  [ECS Express Mode](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/express-service-overview.html),
  which provisions the load balancer, HTTPS URL (`<service>.ecs.<region>.on.aws`) and autoscaling.
- Create once per account and region: the cluster, the ECR repository and the two IAM roles from
  AWS's [Express Mode guide](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/express-service-getting-started.html).
  `apply` checks them first and prints the commands for anything missing; `apply --dry-run` stops after the checks.
- Each service becomes the Express Mode service `<spec name>-<service>`, tagged
  `anyship-project=<spec name>`; `status`, `logs` and `destroy` find it by name, with no local state.
  Built images are tagged with the service name in the repository and deployed by digest.
- Secrets live in Secrets Manager as `anyship/<spec name>/<NAME>` and reach the container as environment
  variables. The execution role needs `secretsmanager:GetSecretValue` on them (`plan` warns). Values are
  handed to the aws CLI through a private temporary file, never on the command line.
- The load balancer checks `healthCheck.path`, or `/`, for HTTP 200. One HTTP port per service
  (default 80); `internal` ports need private `subnets`. `replicas` is the minimum task count.
- `logs` uses `aws logs tail`, so `-f` works for one service at a time; it selects by `--since`, not `-n`.
- `destroy` deletes the services with their load balancers; the cluster, roles, images and log groups
  stay. `destroy --volumes` also deletes the secrets without a recovery window.
- Refused with a reason: static sites, workers, volumes, cron, TCP/UDP ports, `domains`
  (`AWS_DOMAIN_UNSUPPORTED`; point a CNAME at the `on.aws` URL, or add your own ACM certificate and
  listener rule) and resources anyship would have to provision.

[All targets](../../README.md#targets)
