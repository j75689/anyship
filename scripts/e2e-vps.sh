#!/usr/bin/env bash
# End-to-end test of the vps target against a real Linux host with Docker,
# reachable as `localhost` over ssh (CI sets that up on its runner).
#
#   ANYSHIP=./anyship scripts/e2e-vps.sh
set -euo pipefail

ANYSHIP=$(cd "$(dirname "${ANYSHIP:?set ANYSHIP to the anyship binary}")" && pwd)/$(basename "$ANYSHIP")
repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$*"; exit 1; }

cp -R "$repo/dockerfile/testdata/apps/python-flask" "$work/app"
cd "$work/app"

step "init detects the Flask app"
"$ANYSHIP" init
# spec is the last top-level key that init writes, so targets can be appended to it.
printf '  targets:\n    vps:\n      host: localhost\n      dir: anyship-e2e/flask\n' >> anyship.yaml
cat anyship.yaml

step "plan generates a Dockerfile"
"$ANYSHIP" plan -t vps
"$ANYSHIP" plan -t vps --json | jq -e '.ready and (.files | map(.path) | any(endswith("web.Dockerfile")))' >/dev/null \
  || fail "plan should be ready and include the generated Dockerfile"

step "preflight refuses a port that is already taken"
python3 -m http.server 8000 >/dev/null 2>&1 &
squatter=$!
sleep 1
if "$ANYSHIP" apply -t vps --dry-run > preflight.out 2>&1; then
  cat preflight.out
  kill "$squatter"
  fail "dry run should fail while port 8000 is taken"
fi
cat preflight.out
kill "$squatter"
wait "$squatter" 2>/dev/null || true
grep -q "Port 8000/tcp is already in use" preflight.out || fail "expected a port-in-use finding"

step "dry run passes once the port is free"
"$ANYSHIP" apply -t vps --dry-run
[ ! -e ~/anyship-e2e/flask ] || fail "a dry run must not upload anything"

step "apply deploys it"
"$ANYSHIP" apply -t vps --yes
for _ in $(seq 60); do
  if curl -fsS http://localhost:8000/ >/dev/null; then break; fi
  sleep 2
done
[ "$(curl -fsS http://localhost:8000/)" = ok ] || fail "the app should answer on port 8000"

step "status reports it running"
"$ANYSHIP" status -t vps
"$ANYSHIP" status -t vps --json | jq -e '.deployed and all(.services[]; .running == .desired and .state == "running")' >/dev/null \
  || fail "status --json should report every service running"

step "logs come back"
"$ANYSHIP" logs -t vps -n 20 | tee logs.out
grep -qi gunicorn logs.out || fail "expected gunicorn in the logs"

step "a redeploy's preflight skips the project's own ports"
"$ANYSHIP" apply -t vps --dry-run | tee redeploy.out
grep -q "already running" redeploy.out || fail "expected the redeploy note"

step "destroy --dry-run removes nothing"
"$ANYSHIP" destroy -t vps --dry-run
curl -fsS http://localhost:8000/ >/dev/null || fail "the app should still run after a dry run"

step "destroy --volumes removes everything"
"$ANYSHIP" destroy -t vps --volumes --yes
if "$ANYSHIP" status -t vps; then fail "status should fail once destroyed"; fi
[ ! -e ~/anyship-e2e/flask ] || fail "the deployment directory should be gone"
if curl -fsS http://localhost:8000/ >/dev/null 2>&1; then fail "the app should be gone"; fi

step "destroying again is a no-op"
"$ANYSHIP" destroy -t vps --yes | tee again.out
grep -q "Nothing to remove" again.out || fail "expected a not-deployed message"

printf '\n\033[32mvps end-to-end test passed\033[0m\n'
