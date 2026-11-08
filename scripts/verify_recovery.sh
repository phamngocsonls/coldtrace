#!/usr/bin/env bash
# verify_recovery.sh — after remediation, poll until the device reports OK.
# usage: ./scripts/verify_recovery.sh [DEVICE_ID] [TIMEOUT_SECONDS]
set -euo pipefail
DEVICE="${1:-FZ-003}"
TIMEOUT="${2:-300}"
GW="${GATEWAY_URL:-http://localhost:${GATEWAY_PORT:-8099}}"

echo "remediating ${DEVICE}..."
curl -sS -X POST "${GW}/devices/${DEVICE}/remediate"
echo

echo "polling ${GW}/devices/health/${DEVICE} until status=OK (timeout ${TIMEOUT}s)..."
SECS=0
until STATUS=$(curl -sS "${GW}/devices/health/${DEVICE}" | jq -r '.[0].status // "NO_DATA"') && [[ "$STATUS" == "OK" ]]; do
  if [[ $SECS -ge $TIMEOUT ]]; then
    echo "TIMEOUT after ${TIMEOUT}s — last status: ${STATUS}" >&2
    exit 1
  fi
  sleep 15; SECS=$((SECS+15))
  echo "  t+${SECS}s: status=${STATUS}"
done

echo "RECOVERED: ${DEVICE} status=OK at t+${SECS}s"
echo "proof: incidents timeline -> curl ${GW}/incidents ; Datadog monitor should auto-recover"
