#!/usr/bin/env bash
# demo_cleanup.sh — pristine pre-demo state: clear faults, resolve OPEN
# incidents, reset the twin's desired state, and drop rehearsal residue while
# keeping the seeded incident-memory corpus.
#
# usage: ./scripts/demo_cleanup.sh [--keep-history]
#   default       : clear faults, resolve OPEN incidents, delete only
#                   NON-seeded incidents/events (seeded history survives, so
#                   "similar past incidents" is never empty on stage)
#   --keep-history: also keep rehearsal incidents/events for the audit trail
set -euo pipefail
cd "$(dirname "$0")/.."

echo "== ColdTrace demo cleanup =="

# 1. Clear all device faults + reset fleet via ops-api
if curl -s -m 6 -X POST http://localhost:8092/api/demo/reset -o /dev/null; then
  echo "  fleet reset issued"
else
  echo "  WARN: ops-api unreachable; clearing faults directly via broker"
  for d in FZ-001 FZ-002 FZ-003 FZ-004 FZ-005 FZ-006 FZ-007 FZ-008 FZ-009 FZ-010 FZ-011 FZ-012; do
    docker compose exec -T broker sh -c \
      "mosquitto_pub -h localhost -t 'coldchain/${d}/commands' -q 1 -m '{\"action\":\"demo_reset\"}'" >/dev/null 2>&1 || true
  done
fi

# 2. Resolve all OPEN incidents
MONGO="docker compose exec -T mongodb mongosh --quiet coldtrace --eval"
$MONGO 'db.ai_incidents.updateMany({status:"OPEN"},{$set:{status:"RESOLVED",resolvedAt:new Date(),resolvedBy:"system:cleanup"}})' >/dev/null
echo "  OPEN incidents resolved (resolvedBy: system:cleanup)"

# 3. Reset twin desired state to defaults
$MONGO 'db.device_state.updateMany({},{$set:{"desired.setpoint":-18,"desired.updatedAt":new Date(),"desired.updatedBy":"system:cleanup"}})' >/dev/null
echo "  desired setpoints reset to -18C"

# 4. Drop rehearsal residue but keep seeded incident-memory history
if [ "${1:-}" != "--keep-history" ]; then
  $MONGO 'db.ai_incidents.deleteMany({seeded:{$ne:true}})' >/dev/null
  $MONGO 'db.events.deleteMany({})' >/dev/null
  echo "  rehearsal incidents + events cleared (seeded history preserved)"
else
  echo "  history preserved (--keep-history)"
fi

echo "  done. Run scripts/demo_preflight.sh to confirm."
