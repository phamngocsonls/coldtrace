#!/usr/bin/env bash
# inject_incident.sh — start a compressor failure on one device.
# Publishes `enter_compressor_failure` on the device command topic via the
# broker container (no host mosquitto client needed).
#
# usage: ./scripts/inject_incident.sh [DEVICE_ID]   (default FZ-003)
set -euo pipefail
DEVICE="${1:-FZ-003}"

docker compose exec -T broker sh -c \
  "mosquitto_pub -h localhost -t 'coldchain/${DEVICE}/commands' -q 1 \
   -m '{\"action\":\"enter_compressor_failure\",\"issuedBy\":\"sre-console\",\"at\":\"$(date -u +%FT%TZ)\"}'"

echo "incidents injected: ${DEVICE} -> compressor_failure (temp will ramp ~+0.3°C/min)"
echo "watch: Datadog monitors (anomaly first, then threshold) + console 'ColdTrace — Ops Console' (:3301)"
