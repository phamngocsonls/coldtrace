#!/usr/bin/env bash
# demo_preflight.sh — pre-demo health check. Run ~5 min before judging.
# Exits non-zero (with a human-readable fix hint) if anything is broken.
set -u
cd "$(dirname "$0")/.."
FAIL=0
WARN=0
ok()   { printf "  OK   %s\n" "$1"; }
warn() { printf "  WARN %s — %s\n" "$1" "$2"; WARN=$((WARN + 1)); }
bad()  { printf "  FAIL %s — %s\n" "$1" "$2"; FAIL=1; }

echo "== ColdTrace demo preflight =="

# 1. MongoDB: reachable + replica set has a PRIMARY (local mode only —
# when MONGO_URI points at Atlas this check is skipped entirely).
source_dot() { [ -f .env ] && set -a && . ./.env && set +a; }
source_dot
case "${MONGO_URI:-}" in
  mongodb+srv://*|*mongodb.net*)
    ok "MONGO_URI points at Atlas (skipping local RS check)"
    ;;
  *)
    # Verify the RS advertises the Docker service name, not a loopback address.
    # If a member advertises 127.0.0.1/localhost, containers that lose the
    # "mongodb" Docker DNS entry fall back to loopback and report the
    # misleading "dial tcp 127.0.0.1:27017: connection refused".
    MONGO_OUT=$(docker compose exec -T mongodb mongosh --quiet --eval 'try{JSON.stringify({rs: rs.status().members.map(m=>m.stateStr), host: rs.conf().members[0].host, ping: db.adminCommand("ping").ok})}catch(e){JSON.stringify({rs: "standalone"})}' 2>/dev/null | tail -1)
    echo "$MONGO_OUT" | grep -q PRIMARY && ok "MongoDB replica set PRIMARY" || bad "MongoDB replica set" "run: docker compose exec mongodb mongosh --eval 'rs.initiate({_id:\"rs0\",members:[{_id:0,host:\"mongodb:27017\"}]})'"
    # An RS advertising loopback only works because /etc/hosts happens to map
    # the name; catch that before it becomes a confusing runtime failure.
    case "$MONGO_OUT" in
      *'"host":"127.0.0.1'*|*'"host":"localhost'*)
        bad "MongoDB member advertises loopback" "re-initiate with host=\"mongodb:27017\" (see README)" ;;
    esac
    ;;
esac

# 2. Broker healthy
docker compose ps broker --format '{{.Status}}' 2>/dev/null | grep -q healthy && ok "MQTT broker healthy" || bad "MQTT broker" "docker compose up -d broker"

# 3. Core services responding
for ep in "gateway:8099/devices/health" "ops-api:8092/api/health" "ops-api:8092/api/policy"; do
  svc="${ep%%:*}"; rest="${ep#*:}"
  if [ "$svc" = "gateway" ]; then url="http://localhost:8099/${rest#*/}"; else url="http://localhost:8092/${rest#*/}"; fi
  code=$(curl -s -m 4 -o /dev/null -w '%{http_code}' "$url" 2>/dev/null)
  [ "$code" = "200" ] && ok "$svc up" || bad "$svc" "expect $url -> 200, got $code (docker compose up -d --build $svc)"
done

# 4. AI agent + fleet sim publishing (Atlas mode: read freshness via the
# gateway instead of the local mongo container)
LAST_TS=$(docker compose exec -T mongodb mongosh --quiet coldtrace /dev/stdin <<'JS' 2>/dev/null | tail -1
const t = db.telemetry.aggregate([{$sort:{ts:-1}},{$limit:1},{$project:{ts:1}}]).toArray()[0];
print(t && t.ts ? Math.round((Date.now()-t.ts.getTime())/1000) : "none");
JS
)
if [ "$LAST_TS" != "none" ] && [ "$LAST_TS" -lt 30 ] 2>/dev/null; then
  ok "telemetry fresh (last reading ${LAST_TS}s ago)"
else
  bad "telemetry stale or missing" "check fleet-simulator: docker compose logs fleet-simulator"
fi

# 5. Fleet all OK (no leftover rehearsal faults)
FLEET=$(curl -s -m 5 http://localhost:8092/api/fleet 2>/dev/null)
NOT_OK=$(echo "$FLEET" | python3 -c "import json,sys; d=json.load(sys.stdin); print(sum(1 for x in d if x['status']!='OK'))" 2>/dev/null || echo "?")
[ "$NOT_OK" = "0" ] && ok "fleet all OK" || bad "$NOT_OK device(s) not OK" "POST /api/demo/reset, wait 2 min, re-run"

# 6. No stale OPEN incidents
STALE_INC=$(curl -s -m 5 'http://localhost:8092/api/incidents?status=OPEN' 2>/dev/null | python3 -c "
import json,sys
raw=sys.stdin.read()
d=json.loads(raw) if raw.strip() else []
print(len(d) if isinstance(d,list) else 0)" 2>/dev/null || echo "?")
[ "$STALE_INC" = "0" ] && ok "no OPEN incidents" || bad "$STALE_INC OPEN incident(s)" "run scripts/demo_cleanup.sh, then POST /api/demo/reset"

# 7. Chart data flows (DeviceSeries pipeline — was silently broken before)
SERIES=$(curl -s -m 5 'http://localhost:8092/api/devices/FZ-003/series?window=5' 2>/dev/null | python3 -c "
import json,sys
raw=sys.stdin.read()
try:
  d=json.loads(raw)
  pts=[p for p in d if p.get('temp') is not None]
  print(len(pts))
except Exception:
  print('ERR')" 2>/dev/null || echo "?")
if [ "$SERIES" != "0" ] && [ "$SERIES" != "ERR" ] && [ "$SERIES" != "?" ]; then
  ok "series chart data ($SERIES points)"
else
  bad "series chart empty" "GET /api/devices/FZ-003/series returned no temp points"
fi

# 8. HACCP export has at least one excursion row. The audit reads raw
# telemetry (24h TTL); seedhistory plants a historical breach so the page
# demonstrates the export before a live fault has crossed -14C.
CSV_ROWS=$(curl -s -m 5 http://localhost:8092/api/compliance/excursions.csv 2>/dev/null | awk 'END{print NR-1}')
if [ "${CSV_ROWS:-0}" -ge 1 ] 2>/dev/null; then
  ok "compliance CSV ($CSV_ROWS device row(s))"
else
  bad "compliance CSV empty" "run: MONGO_URI=mongodb://localhost:27017 go run ./tools/seedhistory"
fi

# 9. Datadog: Agent running + collector ready. Telemetry is only useful if
# both are up — the collector exports to Datadog and the Agent ships infra
# metrics, container logs and the MongoDB fleet rollup.
if docker compose ps --status running --format '{{.Service}}' 2>/dev/null | grep -qx datadog-agent; then
  ok "Datadog Agent running"
else
  bad "Datadog Agent" "docker compose up -d datadog-agent (needs DD_API_KEY in .env)"
fi
if curl -s -m 4 -o /dev/null -w '%{http_code}' http://localhost:13133/ 2>/dev/null | grep -q 200; then
  ok "otel-collector ready (:13133)"
else
  bad "otel-collector" "docker compose up -d otel-collector; check: docker compose logs otel-collector"
fi

# 9. AI agent engine. Unconfigured is NOT a failure: RCA/chat fall back to a
# deterministic, data-grounded template engine, so the demo still works
# keyless. Only a configured-but-dead engine is a real failure.
LLM=$(curl -s -m 5 http://localhost:8092/api/health 2>/dev/null)
if echo "$LLM" | grep -q '"llm":true'; then
  ok "AI agent engine configured ($(echo "$LLM" | sed -n 's/.*"engine":"\([^"]*\)".*/\1/p'))"
else
  warn "AI engine not configured — deterministic template fallback active" \
       "add ANTHROPIC_API_KEY (or OPENAI_API_KEY + OPENAI_BASE_URL/OPENAI_MODEL) to .env for live RCA"
fi

echo
if [ "$FAIL" = "1" ]; then echo "PREFLIGHT FAILED — fix the items above before the demo."; exit 1; fi
if [ "$WARN" -gt 0 ]; then
  echo "PREFLIGHT PASSED with $WARN warning(s) — go for demo (degraded features noted above)."
  exit 0
fi
echo "PREFLIGHT PASSED — you are go for demo."
