#!/usr/bin/env bash
# setup_local.sh — first-run setup on a fresh machine.
#
# Checks the tooling the demo needs, creates .env if it is missing, builds and
# starts the stack, seeds MongoDB, and prints the URLs to open.
#
# Idempotent: safe to re-run. It never overwrites an existing .env (that file
# holds your keys and is gitignored on purpose).
#
# usage: ./scripts/setup_local.sh [--no-seed]
set -euo pipefail
cd "$(dirname "$0")/.."

SEED=1
[ "${1:-}" = "--no-seed" ] && SEED=0

fail=0
need() {
  if command -v "$1" >/dev/null 2>&1; then
    printf '  ok       %s\n' "$2"
  else
    printf '  MISSING  %s\n' "$2"; fail=1
  fi
}

echo "== Tooling =="
need docker  "docker (Orb Desktop / Docker Desktop)"
need go      "go 1.27+ (builds the services and seeds the DB)"
need jq      "jq (demo scripts)"
need curl    "curl (preflight)"

if ! docker compose version >/dev/null 2>&1; then
  printf '  MISSING  docker compose v2 plugin\n'; fail=1
fi
if [ "$fail" = "1" ]; then
  echo
  echo "Install the missing tools above, then re-run: ./scripts/setup_local.sh"
  exit 1
fi

echo
echo "== Configuration =="
if [ -f .env ]; then
  echo "  .env exists — keeping it (edit by hand; never commit it)"
else
  cp .env.example .env
  echo "  created .env from .env.example"
fi

# Warn (do not fail): the stack runs without keys, telemetry just goes nowhere
# and RCA falls back to the template engine.
if ! grep -qE '^DD_API_KEY=.+' .env; then
  echo "  WARN: DD_API_KEY is empty — Datadog gets no data (set it in .env)"
fi
if ! grep -qE '^(OPENAI_API_KEY|ANTHROPIC_API_KEY|CURSOR_API_KEY)=.+' .env; then
  echo "  WARN: no LLM key — RCA uses the template fallback (fine for a dry run)"
fi

echo
echo "== Build + start =="
docker compose up -d --build

echo
echo "== Wait for MongoDB =="
for i in $(seq 1 60); do
  if docker compose exec -T mongodb mongosh --quiet --eval 'db.adminCommand("ping").ok' 2>/dev/null | grep -q 1; then
    echo "  MongoDB is up"
    break
  fi
  [ "$i" = 60 ] && { echo "  MongoDB did not come up — check: docker compose logs mongodb"; exit 1; }
  sleep 2
done

# Single-node replica set: change streams (the SSE bus) need it. initdb does
# not initiate the RS, so do it here and wait for PRIMARY.
if ! docker compose exec -T mongodb mongosh --quiet --eval 'rs.status().ok' >/dev/null 2>&1; then
  echo "  initiating replica set rs0"
  docker compose exec -T mongodb mongosh --quiet --eval 'rs.initiate({_id:"rs0",members:[{_id:0,host:"mongodb:27017"}]})' >/dev/null 2>&1 || true
fi
for i in $(seq 1 30); do
  if docker compose exec -T mongodb mongosh --quiet --eval 'db.hello().isWritablePrimary' 2>/dev/null | grep -q true; then
    echo "  replica set PRIMARY"
    break
  fi
  sleep 2
done

if [ "$SEED" = "1" ]; then
  echo
  echo "== Seed (collections, indexes, demo history) =="
  # The tools run on the host, so they reach MongoDB through the published port.
  MONGO_URI=mongodb://localhost:27017 go run ./tools/initdb
  MONGO_URI=mongodb://localhost:27017 go run ./tools/seedhistory
fi

echo
echo "== Ready =="
cat <<'EOF'
  Ops Console    http://localhost:3301
  Ops API        http://localhost:8092/api/health
  Gateway        http://localhost:8099/devices/health
  AI agent       http://localhost:8091/ai/incidents
  Datadog        https://app.ap1.datadoghq.com  (dashboard v9y-45k-jsz)

  Go/no-go before any demo:   ./scripts/demo_preflight.sh
  Reset to a clean slate:     ./scripts/demo_cleanup.sh
  Push Dashboards/monitors:   ./datadog/apply-datadog.sh
EOF
