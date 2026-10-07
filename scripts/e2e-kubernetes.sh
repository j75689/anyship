#!/usr/bin/env bash
# End-to-end test of the kubernetes target against a real cluster: the
# current kubectl context (or $ANYSHIP_K8S_CONTEXT), with a registry the
# nodes can pull from at $ANYSHIP_K8S_REPOSITORY. CI runs it on a kind
# cluster with a registry on localhost:5001; on a laptop with OrbStack or
# Docker Desktop, a `registry:2` container published on localhost:5000 does.
#
#   ANYSHIP=./anyship ANYSHIP_K8S_REPOSITORY=localhost:5000/anyship-e2e scripts/e2e-kubernetes.sh
set -euo pipefail

ANYSHIP=$(cd "$(dirname "${ANYSHIP:?set ANYSHIP to the anyship binary}")" && pwd)/$(basename "$ANYSHIP")
repo=$(cd "$(dirname "$0")/.." && pwd)
context=${ANYSHIP_K8S_CONTEXT:-$(kubectl config current-context)}
repository=${ANYSHIP_K8S_REPOSITORY:-localhost:5000/anyship-e2e}
namespace=anyship-e2e
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
answer=$(k run curl --image=curlimages/curl:8.14.1 --rm -i --restart=Never -q -- curl -fsS --retry 10 --retry-all-errors --retry-delay 2 "$url/")
[ "$answer" = ok ] || fail "the app should answer ok at $url, got: $answer"

step "status reports it running"
"$ANYSHIP" status -t kubernetes
"$ANYSHIP" status -t kubernetes --json | jq -e '.deployed and all(.services[]; .running == .desired and .state == "running")' >/dev/null \
  || fail "status --json should report every service running"

step "logs come back"
"$ANYSHIP" logs -t kubernetes -n 20 | tee logs.out
grep -qi gunicorn logs.out || fail "expected gunicorn in the logs"

step "a service taken out of the spec is pruned"
cp anyship.yaml one-service.yaml
python3 - <<'PY'
import pathlib
p = pathlib.Path("anyship.yaml"); s = p.read_text()
p.write_text(s.replace("  targets:\n", "    api:\n      kind: server\n      image: nginx:1.27\n      ports:\n        - port: 80\n          exposure: internal\n  targets:\n", 1))
PY
"$ANYSHIP" apply -t kubernetes --yes
k get deployment app-api -o name || fail "the api service should be deployed"
cp one-service.yaml anyship.yaml
"$ANYSHIP" apply -t kubernetes --yes | tee prune.out
grep -q "app-api pruned" prune.out || fail "expected the api objects to be pruned"
if k get deployment app-api -o name 2>/dev/null; then fail "the api Deployment should be gone"; fi

step "destroy removes the Deployments and Services"
"$ANYSHIP" destroy -t kubernetes --yes
if "$ANYSHIP" status -t kubernetes; then fail "status should fail once destroyed"; fi
[ -z "$(k get deployments,services -l anyship-project=app -o name)" ] || fail "the objects should be gone"

step "destroying again is a no-op"
"$ANYSHIP" destroy -t kubernetes --yes | tee again.out
grep -q "Nothing to remove" again.out || fail "expected a not-deployed message"

printf '\n\033[32mkubernetes end-to-end test passed\033[0m\n'
