#!/usr/bin/env bash
# End-to-end test of the kubernetes target against a real cluster: the
# current kubectl context (or $ANYSHIP_K8S_CONTEXT), with a registry the
# nodes can pull from at $ANYSHIP_K8S_REPOSITORY. CI runs it on a kind
# cluster with a registry at kind-registry:5001; on a laptop with OrbStack or
# Docker Desktop, a `registry:2` container published on localhost:5000 does.
#
# With $ANYSHIP_K8S_INGRESS_CLASS set to the cluster's IngressClass, it also
# deploys a service with a domain and checks that the ingress controller
# routes the host (through the controller's Service, named by
# $ANYSHIP_K8S_INGRESS_URL, http://ingress-nginx-controller.ingress-nginx.svc
# by default). With $ANYSHIP_K8S_LB=1 it requires the cluster to give a
# LoadBalancer Service an address.
#
#   ANYSHIP=./anyship ANYSHIP_K8S_REPOSITORY=localhost:5000/anyship-e2e scripts/e2e-kubernetes.sh
set -euo pipefail

ANYSHIP=$(cd "$(dirname "${ANYSHIP:?set ANYSHIP to the anyship binary}")" && pwd)/$(basename "$ANYSHIP")
repo=$(cd "$(dirname "$0")/.." && pwd)
context=${ANYSHIP_K8S_CONTEXT:-$(kubectl config current-context)}
repository=${ANYSHIP_K8S_REPOSITORY:-localhost:5000/anyship-e2e}
namespace=anyship-e2e
ingress_class=${ANYSHIP_K8S_INGRESS_CLASS:-}
ingress_url=${ANYSHIP_K8S_INGRESS_URL:-http://ingress-nginx-controller.ingress-nginx.svc}
work=$(mktemp -d)
cleanup() {
  rm -rf "$work"
  kubectl --context "$context" delete namespace "$namespace" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$*"; exit 1; }
k() { kubectl --context "$context" -n "$namespace" "$@"; }

cp -R "$repo/dockerfile/testdata/apps/python-flask" "$work/app"
cd "$work/app"

step "init detects the Flask app"
"$ANYSHIP" init
# spec is the last top-level key that init writes, so targets can be appended to it.
printf '  targets:\n    kubernetes:\n      context: %s\n      namespace: %s\n      repository: %s\n' "$context" "$namespace" "$repository" >> anyship.yaml
cat anyship.yaml

step "plan generates a Dockerfile and the manifests"
"$ANYSHIP" plan -t kubernetes
"$ANYSHIP" plan -t kubernetes --json | jq -e '.ready and (.files | map(.path) | (any(endswith("web.Dockerfile")) and any(endswith("manifests.yaml"))))' >/dev/null \
  || fail "plan should be ready and include the generated Dockerfile and manifests"

step "preflight refuses a missing namespace"
kubectl --context "$context" delete namespace "$namespace" --ignore-not-found --wait=true >/dev/null
if "$ANYSHIP" apply -t kubernetes --dry-run > preflight.out 2>&1; then
  cat preflight.out
  fail "dry run should fail while the namespace is missing"
fi
cat preflight.out
grep -q "doesn't exist" preflight.out || fail "expected a missing-namespace finding"

step "dry run passes once the namespace exists"
kubectl --context "$context" create namespace "$namespace"
"$ANYSHIP" apply -t kubernetes --dry-run
[ -z "$(k get deployments -o name)" ] || fail "a dry run must not deploy anything"

step "apply deploys it"
"$ANYSHIP" apply -t kubernetes --yes
url=$("$ANYSHIP" plan -t kubernetes --json | jq -r '.findings[] | select(.code == "K8S_PUBLIC_PORT") | .message' | grep -o 'http://[^ ]*svc:[0-9]*')
echo "service URL inside the cluster: $url"
# A pod that runs curl once; not attached, since a container this short can
# finish before `kubectl run -i` attaches and its output is lost then.
k run curl --image=curlimages/curl:8.14.1 --restart=Never -q -- curl -fsS --retry 10 --retry-all-errors --retry-delay 2 "$url/"
k wait --for=jsonpath='{.status.phase}'=Succeeded pod/curl --timeout=120s || { k logs curl; fail "the curl pod didn't succeed"; }
# The log carries the errors of the attempts that were retried; the answer is the last line.
answer=$(k logs curl | tail -n 1)
k delete pod curl --wait=false >/dev/null
[ "$answer" = ok ] || fail "the app should answer ok at $url, got: $answer"

step "status reports it running"
"$ANYSHIP" status -t kubernetes
"$ANYSHIP" status -t kubernetes --json | jq -e '.deployed and all(.services[]; .running == .desired and .state == "running")' >/dev/null \
  || fail "status --json should report every service running"

step "logs come back"
"$ANYSHIP" logs -t kubernetes -n 20 | tee logs.out
grep -qi gunicorn logs.out || fail "expected gunicorn in the logs"

step "a public TCP port gets a LoadBalancer Service, a service taken out of the spec is pruned"
cp anyship.yaml one-service.yaml
python3 - <<'PY'
import pathlib
p = pathlib.Path("anyship.yaml"); s = p.read_text()
# http-echo listens on 5678, a port no ingress controller's load balancer holds.
p.write_text(s.replace("  targets:\n", "    api:\n      kind: server\n      image: hashicorp/http-echo:1.0\n      ports:\n        - port: 5678\n          protocol: tcp\n  targets:\n", 1))
PY
"$ANYSHIP" apply -t kubernetes --yes | tee lb.out
grep -q "api: .*:5678/tcp (LoadBalancer)" lb.out || fail "expected the LoadBalancer address line for api"
[ "$(k get service app-api -o jsonpath='{.spec.type}')" = LoadBalancer ] || fail "app-api should be a LoadBalancer Service"
if [ "${ANYSHIP_K8S_LB:-}" = 1 ]; then
  for _ in $(seq 30); do
    addr=$(k get service app-api -o jsonpath='{.status.loadBalancer.ingress[0].ip}{.status.loadBalancer.ingress[0].hostname}')
    [ -n "$addr" ] && break
    sleep 2
  done
  [ -n "$addr" ] || fail "the cluster should give app-api a load balancer address"
  "$ANYSHIP" status -t kubernetes | tee lb-status.out
  grep -q "load balancer $addr" lb-status.out || fail "status should show the load balancer address"
fi
cp one-service.yaml anyship.yaml
"$ANYSHIP" apply -t kubernetes --yes | tee prune.out
grep -q "app-api pruned" prune.out || fail "expected the api objects to be pruned"
if k get deployment app-api -o name 2>/dev/null; then fail "the api Deployment should be gone"; fi

if [ -n "$ingress_class" ]; then
  step "a public port with a domain gets an Ingress the controller routes"
  python3 - "$ingress_class" <<'PY'
import pathlib, sys
p = pathlib.Path("anyship.yaml"); s = p.read_text()
s = s.replace("      ports:\n        - port: 8000\n", "      ports:\n        - port: 8000\n      domains: [app.example.test]\n", 1)
s = s.replace("    kubernetes:\n", "    kubernetes:\n      ingressClass: %s\n" % sys.argv[1], 1)
p.write_text(s)
PY
  "$ANYSHIP" plan -t kubernetes | tee ingress-plan.out
  grep -q "routes app.example.test" ingress-plan.out || fail "plan should say the Ingress routes the domain"
  "$ANYSHIP" apply -t kubernetes --yes | tee ingress.out
  grep -q "web: http://app.example.test through ingress class $ingress_class" ingress.out || fail "expected the Ingress line for web"
  [ "$(k get ingress app-web -o jsonpath='{.spec.rules[0].host}')" = app.example.test ] || fail "the Ingress should route the domain"
  k run curl-ingress --image=curlimages/curl:8.14.1 --restart=Never -q -- curl -fsS --retry 15 --retry-all-errors --retry-delay 2 -H "Host: app.example.test" "$ingress_url/"
  k wait --for=jsonpath='{.status.phase}'=Succeeded pod/curl-ingress --timeout=120s || { k logs curl-ingress; fail "the curl pod didn't reach the app through the Ingress"; }
  answer=$(k logs curl-ingress | tail -n 1)
  k delete pod curl-ingress --wait=false >/dev/null
  [ "$answer" = ok ] || fail "the ingress controller should route app.example.test to the app, got: $answer"
  cp one-service.yaml anyship.yaml
  "$ANYSHIP" apply -t kubernetes --yes | tee ingress-prune.out
  grep -q "app-web pruned" ingress-prune.out || fail "expected the Ingress to be pruned once the class is gone"
  if k get ingress app-web -o name 2>/dev/null; then fail "the Ingress should be gone"; fi
fi

step "cron entries become CronJobs that call the app and run commands in its image"
python3 - <<'PY'
import pathlib
p = pathlib.Path("anyship.yaml"); s = p.read_text()
s = s.replace("      ports:\n        - port: 8000\n", "      ports:\n        - port: 8000\n      cron:\n        - schedule: '*/5 * * * *'\n          path: /\n          method: GET\n        - schedule: '0 3 * * *'\n          command: python -c print(42)\n", 1)
p.write_text(s)
PY
"$ANYSHIP" apply -t kubernetes --yes | tee cron.out
grep -q 'CronJob app-web-cron-0' cron.out || fail "plan should list the CronJob"
k get cronjob app-web-cron-0 app-web-cron-1 -o name || fail "both CronJobs should exist"
[ "$(k get cronjob app-web-cron-0 -o jsonpath='{.spec.timeZone}')" = Etc/UTC ] || fail "CronJobs should run in UTC"
# Run each once now rather than waiting for its schedule.
k create job --from=cronjob/app-web-cron-0 cron-path-run >/dev/null
k create job --from=cronjob/app-web-cron-1 cron-command-run >/dev/null
k wait --for=condition=complete job/cron-path-run --timeout=120s || { k logs job/cron-path-run; fail "the path job didn't complete"; }
k wait --for=condition=complete job/cron-command-run --timeout=120s || { k logs job/cron-command-run; fail "the command job didn't complete"; }
[ "$(k logs job/cron-path-run | tail -n 1)" = ok ] || fail "the path job should have got ok from the app"
[ "$(k logs job/cron-command-run | tail -n 1)" = 42 ] || fail "the command job should have printed 42"
"$ANYSHIP" logs -t kubernetes -n 50 | tee cron-logs.out
grep -q "cron-command-run" cron-logs.out || fail "anyship logs should include the jobs' pods"
cp one-service.yaml anyship.yaml
"$ANYSHIP" apply -t kubernetes --yes | tee cron-prune.out
grep -q "app-web-cron-0 pruned" cron-prune.out || fail "expected the CronJobs to be pruned"
if k get cronjob app-web-cron-0 -o name 2>/dev/null; then fail "the CronJob should be gone"; fi

step "destroy removes the Deployments and Services"
"$ANYSHIP" destroy -t kubernetes --yes
if "$ANYSHIP" status -t kubernetes; then fail "status should fail once destroyed"; fi
[ -z "$(k get deployments,services -l anyship-project=app -o name)" ] || fail "the objects should be gone"

step "destroying again is a no-op"
"$ANYSHIP" destroy -t kubernetes --yes | tee again.out
grep -q "Nothing to remove" again.out || fail "expected a not-deployed message"

printf '\n\033[32mkubernetes end-to-end test passed\033[0m\n'
