#!/usr/bin/env bash
# apply-datadog.sh — push dashboard.json + monitors.json to the Datadog org.
#
# Auth: DD_API_KEY (required, write) and DD_APP_KEY (required to read/create).
# Both are read from .env when present; the real environment wins.
#
# PAT tokens (ddpat_…) authenticate as `Authorization: Bearer`, not the legacy
# `application_key=` query parameter — this script sends the Bearer header and
# no longer relies on the query-param form.
#
# The dashboard is sent to the v1 API with v1 widget layout
# ({x,y,width,height}) and without v2-only keys (custom_format, title_size,
# autoscale): this region 404s /api/v2/dashboard and v1 rejects those keys.
#
# usage: ./datadog/apply-datadog.sh
set -euo pipefail

cd "$(dirname "$0")/.."
if [ -f .env ]; then
  set -a; . ./.env; set +a
fi

SITE="${DD_SITE:-datadoghq.com}"
API="https://api.${SITE}"
: "${DD_API_KEY:?DD_API_KEY required}"
: "${DD_APP_KEY:?DD_APP_KEY required (dashboard + monitor authoring)}"

HERE="$(cd "$(dirname "$0")" && pwd)"
AUTH=(-H "Authorization: Bearer ${DD_APP_KEY}")

json_quote() { jq -rn --arg v "$1" '$v|@uri'; }

## Convert the source dashboard (v2-style widget layout) to what v1 accepts.
prepare_dashboard() {
  python3 - "$HERE/dashboard.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
drop = {"custom_format", "title_size", "autoscale"}
out = {"title": d["title"], "description": d.get("description", ""),
       "layout_type": "ordered", "widgets": []}
for w in d["widgets"]:
    defn = {k: v for k, v in w["definition"].items() if k not in drop}
    if defn.get("type") == "query_value":
        defn["precision"] = 0
    lay = w.get("layout") or {}
    item = {"definition": defn}
    if lay:
        item["layout"] = {"x": lay["x"], "y": lay["y"],
                          "width": lay["w"], "height": lay["h"]}
    out["widgets"].append(item)
json.dump(out, sys.stdout)
PY
}

echo "== Dashboard =="
DASH_ID=$(curl -sS "${AUTH[@]}" "${API}/api/v1/dashboard" \
  | jq -r '.dashboards[] | select(.title | startswith("ColdTrace")) | .id' | head -1 || true)
BODY=$(prepare_dashboard)
if [[ -n "${DASH_ID}" && "${DASH_ID}" != "null" ]]; then
  curl -sS "${AUTH[@]}" -H 'Content-Type: application/json' \
    -X PUT "${API}/api/v1/dashboard/${DASH_ID}?api_key=${DD_API_KEY}" -d "${BODY}" \
    | jq -r '.url // empty' | sed 's|^|dashboard updated: https://app.'"${SITE}"'|'
else
  curl -sS "${AUTH[@]}" -H 'Content-Type: application/json' \
    -X POST "${API}/api/v1/dashboard?api_key=${DD_API_KEY}" -d "${BODY}" \
    | jq -r '.url // empty' | sed 's|^|dashboard created: https://app.'"${SITE}"'|'
fi

echo "== Monitors =="
jq -c '.monitors[]' "${HERE}/monitors.json" | while read -r mon; do
  NAME=$(jq -r '.name' <<<"$mon")
  EXISTING=$(curl -sS "${AUTH[@]}" "${API}/api/v1/monitor?name=$(json_quote "${NAME}")" \
    | jq -r ".[] | select(.name==\"${NAME}\") | .id" | head -1)
  BODY=$(jq '{type, query, name, message, tags, options}' <<<"$mon")
  if [[ -n "${EXISTING}" ]]; then
    curl -sS "${AUTH[@]}" -H 'Content-Type: application/json' \
      -X PUT "${API}/api/v1/monitor/${EXISTING}?api_key=${DD_API_KEY}" -d "${BODY}" \
      | jq -r '.name // empty' | sed 's/^/monitor updated: /'
  else
    curl -sS "${AUTH[@]}" -H 'Content-Type: application/json' \
      -X POST "${API}/api/v1/monitor?api_key=${DD_API_KEY}" -d "${BODY}" \
      | jq -r '.name // empty' | sed 's/^/monitor created: /'
  fi
done

echo "done. Open https://app.${SITE}/dashboards and Monitors to verify."
