#!/bin/sh
# loadgen-rest — poll the gateway's Mongo-backed REST endpoints on a loop.
# Each poll produces gateway REST traffic; the aggregate queries keep the
# DB round-trip traces flowing alongside the MQTT ingest path.
# usage: loadgen-rest [gateway_host] [interval_seconds]

GW="${1:-gateway}"
INTERVAL="${2:-${LOADGEN_INTERVAL:-1}}"

endpoints="/devices/health /fleet/status /incidents"
i=0
while :; do
  for ep in $endpoints; do
    # wget in busybox: -q quiet, -S no; use -O /dev/null; code ignored — errors tolerated
    wget -q -O /dev/null "http://${GW}:8080${ep}" 2>/dev/null || true
    i=$((i + 1))
  done
  echo "{\"time\":\"$(date -u +%FT%TZ)\",\"service\":\"loadgen-rest\",\"msg\":\"rest_poll\",\"polls\":${i}}"
  sleep "${INTERVAL}"
done
