# aws target: verification record

> **Status: NOT VERIFIED.** No run against a real AWS account has happened yet. The adapter has only
> been tested against a fake `aws` CLI in `adapters/aws/aws_test.go`. This file is the runbook for the
> live run plus a static review of the destroy path; the result sections are empty on purpose and get
> filled in by whoever runs it. Do not mark the `aws` target `verified` in the README until the
> **Results** and **Residual resources** tables below are filled in.
>
> Blocked on: an AWS sandbox account (access key or SSO profile) with ECS, ECR, ELB, IAM,
> Secrets Manager and CloudWatch Logs permissions.

Tracking issue: OPE-14.

## Why this target needs care

ECS Express Mode provisions an Application Load Balancer, a target group and an HTTPS endpoint for
every service, and anyship never names those resources. An ALB bills about **$0.0225 per hour
(~$16/month) even with no traffic**, so a load balancer that outlives `anyship destroy` is the most
expensive failure mode of any adapter in this repo. The point of the run below is less "does it
deploy" and more "does `destroy` leave anything behind that bills".

## 0. Prerequisites

The machine running the test needs:

| Thing | Check | Note |
| --- | --- | --- |
| `aws` CLI v2 with Express Mode | `aws ecs create-express-gateway-service help` | the Express Mode subcommands are recent; an older v2 fails here |
| `docker` with buildx | `docker buildx version` | only needed because the spec below builds from source |
| Credentials | `aws sts get-caller-identity` | a sandbox account, never a shared one |

Minimum IAM permissions for the test identity: `ecs:*` (Express Mode subcommands included),
`ecr:*` on the test repository, `iam:GetRole` / `iam:PassRole` for the two roles below,
`elasticloadbalancing:Describe*`, `secretsmanager:*` on `anyship/*`, and `logs:*`. The `Describe*`
ELB permissions are not used by anyship but are needed for the residual-resource audit.

Build the binary under test from the commit being verified and record it:

```console
$ git rev-parse HEAD
$ go build -o /tmp/anyship ./cmd/anyship
$ /tmp/anyship version
```

## 1. One-time bootstrap (per account and region)

anyship deliberately creates none of these; `apply`'s preflight only checks they exist and prints
these commands for whatever is missing (`adapters/aws/apply.go:198`). Create them by hand so the
test measures the adapter, not the bootstrap:

```console
$ export AWS_REGION=us-east-1
$ aws ecs create-cluster --cluster-name anyship-verify

$ aws ecr create-repository --repository-name anyship-verify

$ aws iam create-role --role-name ecsTaskExecutionRole \
    --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ecs-tasks.amazonaws.com"},"Action":"sts:AssumeRole"}]}'
$ aws iam attach-role-policy --role-name ecsTaskExecutionRole \
    --policy-arn arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy

$ aws iam create-role --role-name ecsInfrastructureRoleForExpressServices \
    --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ecs.amazonaws.com"},"Action":"sts:AssumeRole"}]}'
$ aws iam attach-role-policy --role-name ecsInfrastructureRoleForExpressServices \
    --policy-arn arn:aws:iam::aws:policy/service-role/AmazonECSInfrastructureRoleforExpressGatewayServices
```

**Write down everything that existed before the test** — the audit in step 7 compares against this
list, and these four resources are expected to survive `destroy`:

```console
$ aws elbv2 describe-load-balancers --query 'LoadBalancers[].LoadBalancerArn' > before-elb.json
$ aws elbv2 describe-target-groups  --query 'TargetGroups[].TargetGroupArn'   > before-tg.json
$ aws ecs list-services --cluster anyship-verify                              > before-svc.json
```

## 2. The spec

First round deploys one hello-world service and nothing else. No secrets, no custom VPC, no
`cpu`/`memory`/`maxTasks`, no second service — those are a later round, so a failure here is
unambiguous.

`hello/anyship.yaml`:

```yaml
apiVersion: anyship/v1alpha1
kind: App
metadata:
  name: anyship-hello
spec:
  services:
    web:
      kind: server
      dockerfile: Dockerfile    # without this anyship tries to generate one
      ports:
        - port: 8080
          protocol: http
      healthCheck:
        path: /
  targets:
    aws:
      region: us-east-1
      cluster: anyship-verify
      repository: anyship-verify
```

`hello/Dockerfile`:

```dockerfile
FROM public.ecr.aws/docker/library/python:3.12-alpine
WORKDIR /app
COPY server.py .
EXPOSE 8080
CMD ["python", "server.py"]
```

`hello/server.py` — a plain 200 on every path, so the load balancer health check on `/` passes, and
one log line per request so step 5 has something to read:

```python
from http.server import BaseHTTPRequestHandler, HTTPServer

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(b"hello from anyship on ecs express mode\n")

HTTPServer(("", 8080), Handler).serve_forever()
```

The ECS service is named `anyship-hello-web` (`<spec name>-<service>`), and the image is pushed to
`anyship-verify:anyship-hello-web` and deployed by digest.

## 3. plan — run, at commit 19adc2d

`plan` calls no AWS API and needs no credentials, so this step is already done. It confirms the spec
above is accepted and produces exactly the two expected actions, with no findings:

```console
$ anyship plan -t aws
Plan for aws

Actions:
  $ image anyship-verify:anyship-hello-web  docker buildx build --push to ECR, deployed by digest
  ↑ ECS Express Mode service anyship-hello-web  in cluster anyship-verify, us-east-1

Generated files:
  ~ .anyship/aws/web.aws.txt

Plan is ready.
```

`--json` reports `"ready": true` and `"findings": []`. Exit code 0. Cost: nothing.

| | |
| --- | --- |
| Result | ✅ clean plan, no findings, 2 actions as expected |
| Problems | none |

`.anyship/aws/web.aws.txt` holds the exact `create-express-gateway-service` command and is the
cheapest place to catch a bad flag before spending money. Note it is written by `apply`, not by
`plan` — `plan` only lists the path — so read it after the dry run in step 4 and paste it here.

## 4. apply

Dry run first. It stops after the preflight checks and changes nothing, so it is the free way to
confirm the bootstrap in step 1 is complete:

```console
$ /tmp/anyship apply -t aws --dry-run
$ /tmp/anyship apply -t aws
```

Expect from `apply`: an `AWS_PREFLIGHT_OK` finding, a `docker login` to ECR, a buildx build and
push, `aws ecs create-express-gateway-service anyship-hello-web`, and a printed
`https://anyship-hello-web.ecs.us-east-1.on.aws` URL. **This is where billing starts** — note the
wall-clock time, because ALB hours are the bulk of the cost.

```console
$ date -u            # apply finished — billing starts here
$ curl -sS https://anyship-hello-web.ecs.us-east-1.on.aws/
```

| | |
| --- | --- |
| Result | _(pending)_ |
| Time from apply to a 200 from the URL | _(pending)_ |
| HTTPS URL served the hello body | _(pending)_ |
| Problems | _(pending)_ |

## 5. status and logs

```console
$ /tmp/anyship status -t aws
$ /tmp/anyship status -t aws --json
$ /tmp/anyship logs -t aws --since 10m
$ /tmp/anyship logs -t aws web -f      # ctrl-c after a few lines
```

Expect `status` to show `web` as `running` 1/1 with the HTTPS URL in the detail column, and exit 0.
Expect `logs` to find the CloudWatch log group Express Mode created and show the request logged by
the `curl` in step 4. Note that `-n` is ignored on this target by design; it should print a note
saying so.

| | |
| --- | --- |
| `status` state and exit code | _(pending)_ |
| `status` showed the URL | _(pending)_ |
| `logs --since` showed the curl request | _(pending)_ |
| `logs -f` streamed | _(pending)_ |
| Problems | _(pending)_ |

## 6. destroy

```console
$ /tmp/anyship destroy -t aws            # confirm at the prompt
$ date -u                                # billing should stop around here
```

Expect the summary to say it deletes the Express Mode service with its load balancer and
autoscaling, and to say the images, cluster, roles and log groups stay. `destroy` returns as soon as
the delete call is accepted and says load balancers drain over a few minutes — **it does not wait**,
so step 7 has to run after the drain, not right away.

| | |
| --- | --- |
| Summary text shown | _(pending)_ |
| Result | _(pending)_ |
| Time until the HTTPS URL stopped answering | _(pending)_ |
| Problems | _(pending)_ |

## 7. Residual resources — the point of this exercise

Run this **twice**: about 10 minutes after `destroy`, and again at least an hour later. The second
pass is the one that catches a load balancer that drained but never got deleted. Diff against the
`before-*.json` files from step 1.

```console
# ECS: no service left in the cluster (INACTIVE is deleted, and acceptable)
$ aws ecs list-services --cluster anyship-verify
$ aws ecs describe-services --cluster anyship-verify --services anyship-hello-web \
    --query 'services[].[serviceName,status]'

# ELB: the expensive one. Nothing new versus before-elb.json / before-tg.json
$ aws elbv2 describe-load-balancers --query 'LoadBalancers[].[LoadBalancerName,State.Code,CreatedTime]'
$ aws elbv2 describe-target-groups  --query 'TargetGroups[].[TargetGroupName,TargetGroupArn]'

# ECR: the repository survives by design; check which image tags are left and their size
$ aws ecr describe-images --repository-name anyship-verify \
    --query 'imageDetails[].[imageTags,imageSizeInBytes,imagePushedAt]'

# IAM: the two roles survive by design; check nothing extra was created
$ aws iam list-roles --query 'Roles[?starts_with(RoleName, `ecs`)].RoleName'
$ aws iam list-roles --query 'Roles[?contains(RoleName, `anyship`)].RoleName'

# CloudWatch Logs: log groups survive by design; note them, they bill for storage
$ aws logs describe-log-groups --query 'logGroups[].[logGroupName,storedBytes]'

# Secrets Manager: this spec has no secrets, so expect nothing under anyship/
$ aws secretsmanager list-secrets --query 'SecretList[].[Name,DeletedDate]'

# Anything still tagged as ours, anywhere in the region
$ aws resourcegroupstaggingapi get-resources --tag-filters Key=anyship-project,Values=anyship-hello
```

| Resource | Expected after destroy | 10 min | 1 h+ |
| --- | --- | --- | --- |
| ECS service `anyship-hello-web` | gone or `INACTIVE` | _(pending)_ | _(pending)_ |
| ECS cluster `anyship-verify` | **stays** (pre-existing) | _(pending)_ | _(pending)_ |
| Application Load Balancer | **gone** — deleted by Express Mode | _(pending)_ | _(pending)_ |
| Target group | **gone** — deleted by Express Mode | _(pending)_ | _(pending)_ |
| ECR image `anyship-hello-web` | **stays** by design — see note below | _(pending)_ | _(pending)_ |
| ECR repository | **stays** (pre-existing) | _(pending)_ | _(pending)_ |
| IAM roles (2) | **stays** (pre-existing) | _(pending)_ | _(pending)_ |
| CloudWatch log group | **stays** by design | _(pending)_ | _(pending)_ |
| Secrets under `anyship/anyship-hello/` | none created | _(pending)_ | _(pending)_ |
| Tagged resources | none | _(pending)_ | _(pending)_ |

Then delete the bootstrap by hand and confirm the account is empty again:

```console
$ aws ecr delete-repository --repository-name anyship-verify --force
$ aws ecs delete-cluster --cluster anyship-verify
$ aws logs delete-log-group --log-group-name <from above>
$ aws iam detach-role-policy --role-name ecsTaskExecutionRole --policy-arn arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy
$ aws iam delete-role --role-name ecsTaskExecutionRole
$ aws iam detach-role-policy --role-name ecsInfrastructureRoleForExpressServices --policy-arn arn:aws:iam::aws:policy/service-role/AmazonECSInfrastructureRoleforExpressGatewayServices
$ aws iam delete-role --role-name ecsInfrastructureRoleForExpressServices
```

## 8. What it cost

Read the real number from Cost Explorer with a one-day granularity, two days after the test — AWS
reports usage with a lag, so a same-day check reads low:

```console
$ aws ce get-cost-and-usage --time-period Start=<test day>,End=<next day> \
    --granularity DAILY --metrics UnblendedCost --group-by Type=DIMENSION,Key=SERVICE
```

Rough expectation for a one-hour test in `us-east-1`, as a sanity check on the real figure, not a
substitute for it:

| Item | Rate | ~1 h |
| --- | --- | --- |
| Application Load Balancer | $0.0225/h + LCU | ~$0.03 |
| Fargate task (0.25 vCPU, 0.5 GB) | $0.04048/vCPU-h + $0.004445/GB-h | ~$0.01 |
| ECR storage | $0.10/GB-month | <$0.01 |
| CloudWatch Logs | $0.50/GB ingest | <$0.01 |
| Data transfer, ECR pulls | | <$0.01 |
| **Total** | | **~$0.05** |

| | |
| --- | --- |
| Test window (UTC) | _(pending)_ |
| Actual cost from Cost Explorer | _(pending)_ |
| Anything unexpected on the bill | _(pending)_ |

## Static review of the destroy path

Read from the code before the live run, so the run knows what to look for. These are expectations to
confirm or refute, not findings.

1. **`destroy` only deletes the Express Mode service.** `Destroy` in
   `adapters/aws/lifecycle.go:230` calls `aws ecs delete-express-gateway-service` per service and
   nothing else. The load balancer and target group are deleted by AWS as part of that call, not by
   anyship. The whole cost risk of this target rests on that AWS behaviour, and no test in this repo
   can prove it — only step 7 can.

2. **`destroy` does not wait and does not verify.** It returns right after the delete call is
   accepted ("load balancers drain over a few minutes"). If AWS accepts the delete but fails to tear
   the load balancer down, anyship reports success and the user is billed with nothing on screen to
   suggest a problem. Whether this deserves a `--wait` or a post-delete check depends on what step 7
   finds.

3. **A service already `DRAINING` is reported as "Nothing to remove" — confirmed, needs no AWS
   account.** `adapters/aws/lifecycle.go:241-265` skips services whose status is `DRAINING`, leaving
   `removed` at 0, which falls into the `Nothing to remove` branch. Driving `Destroy` with the
   existing fake CLI and a single `DRAINING` service gives:

   ```
   OK=true  messages="Nothing to remove: shop has no services in cluster default (us-east-1)."
   delete-express-gateway-service calls = 0
   ```

   Running `destroy` twice — the natural thing to do when unsure whether the first one worked — so
   reports that nothing exists while the load balancer is still up and billing. `status` is honest
   about the same state and reports `deleting` (`serviceState`, `lifecycle.go:133`), so the two
   commands contradict each other. Filed separately.

4. **ECR images and IAM roles are kept on purpose.** `DestroySummary`
   (`adapters/aws/lifecycle.go:227`) states it, and the README says the same. anyship never creates
   the repository or the roles — the preflight requires them to already exist — so it does not delete
   them. The OPE-14 acceptance criteria ask for confirmation that `destroy` cleans the ECR image and
   the IAM role, which this design does not do. The table in step 7 therefore records them as
   expected survivors; a decision to actually delete them would be a behaviour change, not a bug fix.
   ECR image storage does bill ($0.10/GB-month), so note the leftover image size either way.

5. **Only secrets are deleted by `--volumes`,** without a recovery window
   (`--force-delete-without-recovery`). The first-round spec has no secrets, so this path is
   untested by this run; it needs a second round.

## Bugs found

One issue per bug, per the acceptance criteria.

| Issue | Summary | Found by |
| --- | --- | --- |
| OPE-19 | `destroy` on a `DRAINING` service says "Nothing to remove", while the load balancer is still billing | static review + fake CLI, no account needed |
| _(pending)_ | anything the live run turns up | |
