# gcp target: verification record

> **Status: NOT VERIFIED.** No run against a real Google Cloud project has happened yet. The adapter
> has only been tested against a fake `gcloud` CLI in `adapters/gcp/gcp_test.go`. This file is the
> runbook for the live run plus a static review of the destroy path. The steps that need no
> credentials are already run and their real output is pasted in; the rest of the result sections are
> empty on purpose and get filled in by whoever runs it. Do not mark the `gcp` target `verified` in
> the README until the **Results** and **Residual resources** tables below are filled in.
>
> Blocked on: a Google Cloud project with billing enabled, plus either a service account JSON key or
> a machine with `gcloud auth login` already done, holding the roles in step 0.

Tracking issue: OPE-13. The AWS equivalent is [aws.md](aws.md).

## Why this target needs care

Cloud Run itself is cheap to leave running by accident: it scales to zero, so an idle service bills
nothing. The cost risk of this target is the other end — **`anyship destroy` keeps the image in
Artifact Registry on purpose** (`DestroySummary`, `adapters/gcp/lifecycle.go:152`), and registry
storage bills every month whether or not anything pulls the image. So the question this run has to
answer is not "did it deploy" but "after `destroy`, what is still there and what does it cost".

The second risk is narrower but worse: `destroy` finds services by the label
`anyship-project=<spec name>`. Anything that lost that label is invisible to `destroy`, and `destroy`
then reports success. Step 8 therefore audits by listing **every** Cloud Run service in the region,
not only the labelled ones.

## Scope of the first round

One service, one HTTP port, one env var, built from a committed Dockerfile. No secrets, no
`private`, no internal ingress, no `replicas`, no second service.

**No `resources` at all.** `checkService` (`adapters/gcp/gcp.go:233`) returns a blocking
`GCP_RESOURCE` error finding for any resource that is not marked `external`, because anyship does not
provision Cloud SQL or anything like it. Putting a resource in the first-round spec would stop
`plan` before it reaches Cloud Run and prove nothing. Secrets, `private`, internal ingress,
`replicas` and multi-service deploys are a later round.

## 0. Prerequisites

The machine running the test needs:

| Thing | Check | Note |
| --- | --- | --- |
| `gcloud` | `gcloud version` | 569.0.0 was used for the static flag review |
| `docker` with buildx | `docker buildx version` | needed because the spec below builds from source; **the Docker daemon must be running**, not just installed |
| Credentials | `gcloud auth list` | a scratch project, never a shared one |
| Billing | `gcloud billing projects describe <project>` | `billingEnabled: true` |

anyship drives the `gcloud` that is on `$PATH` with its current login (`adapters/gcp/apply.go:31`),
so whatever `gcloud config list` shows is what gets used. With a service account key, activate it
first:

```console
$ gcloud auth activate-service-account --key-file key.json
$ gcloud config set project <project>
```

### Minimum roles

Granted on the project, to the identity that runs anyship. Not Owner. The right-hand column is the
reason the role is needed, traced to the command anyship actually runs; the **Confirmed** column is
filled in by the live run, because a permission list that nobody has tested is a guess.

| Role | Needed for | Confirmed |
| --- | --- | --- |
| `roles/serviceusage.serviceUsageViewer` | preflight runs `gcloud services list --enabled` (`apply.go:134`) to check the required APIs | _(pending)_ |
| `roles/run.admin` | `gcloud run deploy`, `run services list/describe/delete`. **Admin, not developer:** a public service is made public with `--allow-unauthenticated`, which sets an IAM policy on the service, and `run.services.setIamPolicy` is not in `roles/run.developer` | _(pending)_ |
| `roles/iam.serviceAccountUser` | Cloud Run runs the service as a service account, and deploying as that account needs `iam.serviceAccounts.actAs` | _(pending)_ |
| `roles/artifactregistry.writer` | `docker push` of the built image | _(pending)_ |
| `roles/logging.viewer` | `gcloud run services logs read` reads Cloud Logging, not Cloud Run | _(pending)_ |

Only for other steps, and better held by a different identity than the one under test:

| Role | Needed for |
| --- | --- |
| `roles/serviceusage.serviceUsageAdmin` | the one-time `gcloud services enable` in step 1 |
| `roles/artifactregistry.admin` | creating the repository in step 1; `repoAdmin` is enough to delete images in step 8 |
| `roles/secretmanager.admin` | a later round only — this spec has no secrets |
| `roles/resourcemanager.projectDeleter` | deleting the scratch project at the end of step 8 |

Build the binary under test from the commit being verified and record it:

```console
$ git rev-parse --short HEAD
19adc2d
$ go build -o /tmp/anyship ./cmd/anyship
$ /tmp/anyship --version
anyship version 0.2.1-0.20261002062437-19adc2d9b46e
```

## 1. One-time bootstrap (per project and region)

anyship creates none of this. Preflight only checks it exists and prints the command for whatever is
missing (`adapters/gcp/apply.go:148`), so create it by hand — then the test measures the adapter and
not the bootstrap.

**Use a throwaway project.** Deleting the project is the only teardown that provably leaves nothing
billing, including the Artifact Registry image that `destroy` keeps:

```console
$ gcloud projects create anyship-verify-ope13
$ gcloud billing projects link anyship-verify-ope13 --billing-account <ACCOUNT_ID>
$ gcloud services enable run.googleapis.com artifactregistry.googleapis.com \
    --project anyship-verify-ope13
$ gcloud artifacts repositories create anyship --repository-format docker \
    --location us-central1 --project anyship-verify-ope13
```

`secretmanager.googleapis.com` is deliberately left off: preflight only requires it when the spec has
secrets (`apply.go:144`), and this one has none. If the dry run in step 4 demands it anyway, that is
a bug — record it.

**Write down what existed before the test.** Step 8 compares against this list:

```console
$ export P=anyship-verify-ope13 R=us-central1
$ gcloud run services list --region $R --project $P                      > before-run.txt
$ gcloud artifacts docker images list $R-docker.pkg.dev/$P/anyship \
    --include-tags --project $P                                          > before-images.txt
$ gcloud secrets list --project $P                                       > before-secrets.txt
$ gcloud iam service-accounts list --project $P                          > before-sa.txt
```

On a fresh project `before-sa.txt` is the interesting one. Cloud Run runs services as the Compute
Engine default service account, `<PROJECT_NUMBER>-compute@developer.gserviceaccount.com`, which
Google creates when the Compute Engine API is first enabled — not by anyship, and possibly not yet on
a brand-new project. **Expectation to confirm, not a known fact:** either the first `gcloud run
deploy` makes Google create it, or the deploy fails asking for a runtime service account. Whichever
happens, write it down — it decides whether the bootstrap above is complete.

## 2. The spec

`hello/anyship.yaml`:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/j75689/anyship/main/schema/anyship.schema.json
apiVersion: anyship/v1alpha1
kind: App
metadata:
  name: anyship-hello
spec:
  services:
    web:
      kind: server
      dockerfile: Dockerfile   # without this anyship generates a Dockerfile instead
      env:
        GREETING: OPE-13       # proves --set-env-vars reaches the container
      ports:
        - name: http
          port: 8080
          protocol: http
          exposure: public
  targets:
    gcp:
      project: anyship-verify-ope13
      region: us-central1
      repository: anyship
```

`hello/main.go` — answers 200 on every path, reads `$PORT` the way Cloud Run requires, echoes
`$GREETING`, and logs one line per request so step 6 has something to read:

```go
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		fmt.Fprintf(w, "hello from anyship on cloud run: %s\n", os.Getenv("GREETING"))
	})
	log.Printf("listening on %s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
```

`hello/go.mod` is `module hello` plus `go 1.25`. `hello/Dockerfile`:

```dockerfile
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -o /hello .

FROM gcr.io/distroless/static-debian12
COPY --from=build /hello /hello
EXPOSE 8080
ENTRYPOINT ["/hello"]
```

The Cloud Run service is named `anyship-hello-web` (`<spec name>-<service>`), the image goes to
`us-central1-docker.pkg.dev/anyship-verify-ope13/anyship/anyship-hello-web`, and the deploy uses the
digest, not a tag.

## 3. plan — run, at commit 19adc2d

`plan` calls no Google API and needs no credentials, so this step is done. It confirms the spec above
is accepted and produces exactly the two expected actions, with no findings:

```console
$ /tmp/anyship validate
✔ anyship-hello: 1 service(s) (web), 0 resource(s)

$ /tmp/anyship plan -t gcp

Plan for gcp

Actions:
  $ image us-central1-docker.pkg.dev/anyship-verify-ope13/anyship/anyship-hello-web  docker buildx build --push, deployed by digest
  ↑ Cloud Run service anyship-hello-web  in anyship-verify-ope13/us-central1

Generated files:
  ~ .anyship/gcp/web.gcloud.txt

Plan is ready.
```

Exit code 0 for both. Cost: nothing.

| | |
| --- | --- |
| Result | ✅ clean plan, no findings, 2 actions as expected |
| Problems | none |

## 4. apply --dry-run — the failure path is run; the success path is not

The dry run stops after the preflight checks and changes nothing (`apply.go:70`), so it is the free
way to prove the bootstrap in step 1 is complete. Note that `apply` writes the generated files
**before** the checks, so the generated `gcloud` command is on disk even when the dry run fails.

Pointed at a project that does not exist, on this machine, the output was:

```console
$ /tmp/anyship apply -t gcp --dry-run -y
...
checking project anyship-verify-ope13 in us-central1
ERROR: (gcloud.services.list) [j75689@gmail.com] does not have permission to access projects instance
[anyship-verify-ope13] (or it may not exist): Project 'anyship-verify-ope13' not found or permission denied.
...
Checks on the target:
  ✖ Couldn't read project anyship-verify-ope13; check that it exists and that you can access it.

✖ Preflight checks failed; nothing was changed.
$ echo $?
1
```

That is the `GCP_PREFLIGHT_PROJECT` branch behaving correctly: the missing project is caught before
anything is created, and exit 1 is correct. It also means a wrong project id or a missing role shows
up here, for free, before any money is spent.

The same run wrote `.anyship/gcp/web.gcloud.txt`, which is the cheapest place to catch a bad flag:

```console
# Generated by anyship: the deploy command for this service.
gcloud run deploy anyship-hello-web \
  --image 'us-central1-docker.pkg.dev/anyship-verify-ope13/anyship/anyship-hello-web@<digest from the build>' \
  --region us-central1 --project anyship-verify-ope13 --port 8080 \
  --labels anyship-project=anyship-hello,anyship-service=web \
  --ingress all --allow-unauthenticated --set-env-vars GREETING=OPE-13 --clear-secrets --quiet
```

Every flag in that line exists in the GA surface of gcloud 569.0.0 — checked one by one against the
installed CLI's help, including `gcloud run services logs read`, which turned out to be GA and not
beta-only. So a flag that simply does not exist is ruled out. What is **not** ruled out is Cloud Run
rejecting a *value*, which is what the rest of this runbook is for.

With a real project, expect instead:

```console
$ /tmp/anyship apply -t gcp --dry-run -y
checking project anyship-verify-ope13 in us-central1
Checks on the target:
  • Project anyship-verify-ope13 is ready: logged in and the required APIs are enabled.
✔ Dry run: preflight checks passed for anyship-verify-ope13; nothing was changed.
```

Keyword to look for: `GCP_PREFLIGHT_OK` / "is ready", and exit 0.

| | |
| --- | --- |
| Dry run against the real project | _(pending)_ |
| Preflight demanded anything step 1 missed | _(pending)_ |
| Problems | _(pending)_ |

## 5. apply

```console
$ date -u                              # billing starts somewhere after here
$ /tmp/anyship apply -t gcp -y
```

Expect, in order: the preflight `is ready` line, `docker login us-central1-docker.pkg.dev`, a buildx
build and push, `gcloud run deploy anyship-hello-web`, then a printed `*.run.app` URL. Google has
used more than one URL format over time (`anyship-hello-web-<hash>-uc.a.run.app` and
`anyship-hello-web-<project number>.us-central1.run.app`), so record the one you actually get rather
than matching a pattern. The final line should be
`Deployed anyship-hello to Cloud Run in anyship-verify-ope13/us-central1.` followed by `web: <url>`.

Then prove the container really serves, with the env var it was given:

```console
$ curl -sS <the URL apply printed>
hello from anyship on cloud run: OPE-13
```

A 403 instead means `--allow-unauthenticated` did not take effect — most likely an org policy that
forbids granting `roles/run.invoker` to `allUsers`. That is an environment limit, not an adapter bug,
but record which one it was.

| | |
| --- | --- |
| Result and exit code | _(pending)_ |
| Time from `apply` to a 200 from the URL | _(pending)_ |
| Body matched `GREETING` | _(pending)_ |
| Image digest deployed | _(pending)_ |
| Problems | _(pending)_ |

## 6. status and logs

```console
$ /tmp/anyship status -t gcp
$ /tmp/anyship status -t gcp --json
$ /tmp/anyship logs -t gcp
$ /tmp/anyship logs -t gcp --since 10m
$ /tmp/anyship logs -t gcp -n 20
$ /tmp/anyship logs -t gcp -f          # expected to be refused, with a pointer
```

Expect `status` to show `web` as `running` 1/1 with the URL in the detail column, and
`anyship-verify-ope13/us-central1` as the location. Expect `logs` to show the request logged by the
`curl` in step 5 plus the `listening on 8080` startup line. `logs -f` is refused by design
(`lifecycle.go:102`) and must point at `gcloud beta run services logs tail`; check the message names
the right service and region.

Cloud Logging has an ingest lag of a few seconds to a minute, so an empty first `logs` run is not
automatically a bug — retry once before recording it as one.

| | |
| --- | --- |
| `status` state and exit code | _(pending)_ |
| `status` showed the URL | _(pending)_ |
| `logs` showed the curl request | _(pending)_ |
| `logs --since 10m` worked | _(pending)_ |
| `logs -n 20` honoured the limit | _(pending)_ |
| `logs -f` refused with the right pointer | _(pending)_ |
| Problems | _(pending)_ |

## 7. destroy

The summary is printed before the confirmation prompt and needs no credentials, so its exact wording
is already known:

```console
$ /tmp/anyship destroy -t gcp --dry-run
Destroy anyship-hello on gcp:
  • Delete the Cloud Run services anyship-hello-web in anyship-verify-ope13/us-central1.
  • Keep the images in Artifact Registry.
```

`--volumes` adds nothing here, because this spec has no secrets — correct, and worth noting so the
second round can check the secret line appears when there is one.

```console
$ /tmp/anyship destroy -t gcp         # answer the prompt
$ date -u
$ curl -sS -o /dev/null -w '%{http_code}\n' <the URL from step 5>
```

Expect `Removed anyship-hello from Cloud Run in anyship-verify-ope13/us-central1.` and the URL to
stop answering. Deleting a Cloud Run service deletes its revisions with it, so no revision should
survive — step 8 checks that rather than assuming it.

| | |
| --- | --- |
| Summary text shown | ✅ recorded above, from `--dry-run` |
| Result | _(pending)_ |
| Time until the URL stopped answering | _(pending)_ |
| Problems | _(pending)_ |

## 8. Residual resources — the point of this exercise

Run this after `destroy`. Diff against the `before-*.txt` files from step 1.

```console
$ export P=anyship-verify-ope13 R=us-central1

# Cloud Run: list EVERY service in the region, not only the labelled ones.
# destroy only deletes services carrying anyship-project=<spec>, so a service that
# lost its label is exactly what this check is for.
$ gcloud run services list --region $R --project $P
$ gcloud run services list --project $P                     # all regions, in case of a region typo
$ gcloud run revisions list --region $R --project $P

# Artifact Registry: the image SURVIVES by design. Record the tags and the size,
# because this is the only thing here that keeps billing.
$ gcloud artifacts docker images list $R-docker.pkg.dev/$P/anyship \
    --include-tags --project $P
$ gcloud artifacts repositories describe anyship --location $R --project $P
# read the repository size out of that output; the image size is what bills

# Secret Manager: this spec has no secrets, so expect nothing at all.
$ gcloud secrets list --project $P

# Service accounts: anyship creates none. Expect only what before-sa.txt had, plus
# possibly the Compute Engine default account if Google created it during apply.
$ gcloud iam service-accounts list --project $P

# Cloud Logging: log entries survive their service and bill for storage after the
# free allowance. Note the buckets' retention.
$ gcloud logging buckets list --location global --project $P

# Anything else still tagged as ours anywhere in the project (needs the Cloud Asset
# API enabled; skip it rather than enabling an API just for the audit)
$ gcloud asset search-all-resources --scope projects/$P \
    --query 'labels.anyship-project=anyship-hello'
```

| Resource | Expected after destroy | Found |
| --- | --- | --- |
| Cloud Run service `anyship-hello-web` | **gone** | _(pending)_ |
| Cloud Run revisions | **gone** with the service | _(pending)_ |
| Any other Cloud Run service in the region | none | _(pending)_ |
| Artifact Registry image `anyship-hello-web` | **stays** by design — record size | _(pending)_ |
| Artifact Registry repository `anyship` | **stays** (pre-existing) | _(pending)_ |
| Secret Manager secrets | none created | _(pending)_ |
| Service accounts | none created by anyship | _(pending)_ |
| Cloud Logging entries | **stay** until retention expires | _(pending)_ |
| Enabled APIs | **stay** — anyship never enables or disables them | _(pending)_ |

Then delete the leftovers by hand, and record what each command had to remove — that is the real
answer to "what does `destroy` leave behind":

```console
$ gcloud artifacts docker images delete \
    $R-docker.pkg.dev/$P/anyship/anyship-hello-web --delete-tags --project $P
$ gcloud artifacts repositories delete anyship --location $R --project $P
```

Finally delete the whole scratch project, which is the only teardown that can be proved complete:

```console
$ gcloud projects delete anyship-verify-ope13
```

Deletion is scheduled, not immediate — Google keeps the project for about 30 days and it can be
restored in that window. Confirm it is at least scheduled:

```console
$ gcloud projects describe anyship-verify-ope13 --format 'value(lifecycleState)'
DELETE_REQUESTED
```

| | |
| --- | --- |
| Project deletion requested | _(pending)_ |
| Anything that needed manual deletion | _(pending)_ |

## 9. What it cost

Read the real number from the billing export or the console two days after the test; Google reports
usage with a lag, so a same-day check reads low.

```console
$ gcloud billing accounts list
# then: console.cloud.google.com/billing/<account>/reports, filtered to the project
```

Rough expectation for a one-hour test in `us-central1`, as a sanity check on the real figure and not
a substitute for it:

| Item | Rate | ~1 h |
| --- | --- | --- |
| Cloud Run requests, CPU, memory | first 2M requests and 180k vCPU-s per month are free | $0.00 — scales to zero between requests |
| Artifact Registry storage | $0.10/GB-month, first 0.5 GB free | <$0.01 — one image of roughly 10-20 MB |
| Cloud Build | not used — the image is built locally with buildx | $0.00 |
| Secret Manager | no secrets in this round | $0.00 |
| Network egress | a handful of `curl` responses | <$0.01 |
| Cloud Logging | first 50 GB/month free | $0.00 |
| **Total** | | **under $0.05, most likely $0.00** |

How to keep it there: one service, one region (`us-central1`, a Tier-1 region with the lowest Cloud
Run rate), no `replicas` so no warm instance is held, no `cpu`/`memory` overrides, `destroy`
immediately after step 6, and the image deleted in step 8 rather than left in the registry.

| | |
| --- | --- |
| Test window (UTC) | _(pending)_ |
| Actual cost from billing | _(pending)_ |
| Anything unexpected on the bill | _(pending)_ |

## Static review of the destroy path

Read from the code before the live run, so the run knows what to look for. These are expectations to
confirm or refute, not findings — except where marked confirmed.

1. **`destroy` deletes Cloud Run services and nothing else.** `Destroy`
   (`adapters/gcp/lifecycle.go:155`) calls `gcloud run services delete` for each service it finds by
   label, and `gcloud secrets delete` only with `--volumes`. It never touches the image, the
   repository, the APIs or any service account. `DestroySummary` says "Keep the images in Artifact
   Registry", so this is deliberate, and the README says the same.

   The OPE-13 acceptance criteria ask whether `destroy` clears the Artifact Registry image. By
   design it does not. Step 8 records it as an expected survivor with its size; changing that would
   be a behaviour change, not a bug fix.

2. **`destroy` finds services only by label.** `deployed` (`lifecycle.go:36`) filters on
   `metadata.labels.anyship-project=<spec name>`. The filter runs server side, so a service without
   that label is simply absent from the result and is never deleted. Re-deploying by hand with
   `gcloud run deploy` and no `--labels` is enough to produce that state. This is why step 8 lists
   every service in the region instead of trusting `destroy`'s own view.

3. **`destroy --volumes` reports success when it removed nothing — confirmed, no credentials
   needed.** `lifecycle.go:188` only takes the "Nothing to remove" branch when `removed == 0 && !opts.Volumes`.
   With `--volumes` and nothing deployed, `removed` stays 0, the branch is skipped, and the message
   claims a removal. Driving `Destroy` with the existing fake CLI, an empty service list and a spec
   with no secrets gives:

   ```
   OK=true  messages=["Removed shop from Cloud Run in my-project/us-central1."]
   calls:   gcloud run services list ...        (one call, no delete at all)
   ```

   So `destroy --volumes` on an empty project says it removed something it never had. It is a wrong
   message rather than a wrong action, but it matters exactly here: this runbook asks the operator to
   trust `destroy`'s output as evidence that teardown worked. Filed as OPE-32.

   `status` on the same state is honest and reports `missing`, so the two commands disagree.

4. **Deleting a service deletes its revisions.** Cloud Run ties revisions to the service, so no
   explicit revision cleanup exists in the adapter. Step 8 checks it rather than assuming it.

5. **`--volumes` deletes secrets with no recovery window.** `gcloud secrets delete`
   (`lifecycle.go:183`) is immediate and the value cannot be recovered; `DestroySummary` warns about
   it in capitals. The first round has no secrets, so this path stays untested until round two.

6. **`apply` is not transactional.** Secrets are written, then the image is pushed, then services are
   deployed one by one (`apply.go:74-113`). A failure at the deploy step leaves the pushed image, and
   `destroy` will not remove it. Nothing is wrong with that, but it means a failed `apply` also needs
   the step 8 audit.

## Bugs found

One issue per bug, per the acceptance criteria.

| Issue | Summary | Found by |
| --- | --- | --- |
| OPE-32 | `destroy --volumes` prints "Removed ... from Cloud Run" when it deleted nothing | static review + fake CLI, no project needed |
| _(pending)_ | anything the live run turns up | |

The flag-level review of all 20+ `gcloud` flags the adapter sends found nothing wrong, so no issue
was opened for it.
