# ColdTrace Demo Runbook

## 1. Bring-up (one command, ~2 min)

```bash
docker compose up -d --build --wait          # full stack incl. Datadog (9 services)
docker compose --profile loadgen up -d       # optional load generator
MONGO_URI=mongodb://localhost:27017 go run ./tools/initdb  # collections, indexes, devices
MONGO_URI=mongodb://localhost:27017 go run ./tools/seedhistory     # demo history: resolved
                                                                   # incidents (AI incident memory)
                                                                   # + 6d hourly / 21d daily tiers
                                                                   # + a seeded raw excursion (FZ-009) so the
                                                                   # HACCP CSV has rows on day one
```

Datadog dashboard + monitors (`./datadog/apply-datadog.sh`) are **optional and
currently cut** from the demo: applying them requires `DD_APP_KEY`, which
`.env` does not have, so they were never pushed. Traces, metrics and logs DO
flow with `DD_API_KEY` alone (verified: 0 export errors) — the demo anchors on
the APM trace chain + log correlation instead. To enable the dashboard later,
add `DD_APP_KEY` to `.env` and run the script.

**Recording a demo video?** Follow `docs/demo-video-plan.md` — scene-by-scene
storyboard with measured timings, edit tricks to hide detection waits, and
per-scene fallbacks.

## 2. Pre-demo checks (5 min before judging)

```bash
./scripts/demo_preflight.sh   # all 12 checks must pass; exits non-zero otherwise
./scripts/demo_cleanup.sh     # if any rehearsal residue: resolves incidents, resets twins
```

If a Docker daemon restart killed the stack overnight (it does — `mongodb` and
`broker` vanish, `gateway`/`rollup` crash-loop, and `ai-agent` goes SILENT: its
telemetry-poll loops do not resume on their own):

```bash
docker compose up -d mongodb broker          # core back first
docker compose restart ai-agent              # ALWAYS — silent-loop recovery
docker compose up -d datadog-agent otel-collector
./scripts/demo_preflight.sh                  # then prove it
```

Preflight covers: Mongo replica set (change streams), broker, gateway, ops-api
(health + policy), telemetry freshness, fleet all-OK, no stale OPEN incidents,
series data present, Datadog Agent running, otel-collector ready, AI agent LLM
engine configured.

**Datadog** — the only telemetry backend: traces, metrics and logs arrive from
`otelcol/config.yaml` and are tagged `env:hackathon`; the fleet rollup comes
from the Agent's MongoDB custom query.

## 3. Console — http://localhost:3301

| Page | Story |
|---|---|
| Fleet Overview | KPI strip, live device grid, Sites rollup ($facet/$setWindowFields), event feed |
| Device detail | uPlot temp trend, twin state, controls (setpoint/defrost/reboot) |
| Incidents | AI decision cards with **topFactors explainability bars** (weight × z per feature), guardrail gates, LLM RCA (the configured model via OneNexus), trace link to Datadog |
| AI Control | autonomy approvals per class, guardrail policy editor, kill switch, **Model Health** (confusion matrix + accuracy) |
| Events | filterable audit trail + **operator-authored alert rules** (30s evaluator) |
| Compliance | HACCP excursion CSV export |

## 4. Ops Console — 10-minute winning script (timings measured)

Detection is not instant: the classifier needs a warm ~6-minute window
(72 samples @ 5 s) plus 2–4 minutes of fault physics before it is confident,
so an incident lands **~2.5–4 minutes after injection** (up to ~6.5 min right
after an ai-agent restart or `/api/demo/reset` — both clear the windows).
The wait is **enforced by code**: the evaluation loop skips any device whose
window is not full (a partial window is out-of-distribution and the model is
confidently wrong on it — that rule exists because a 28-sample window once
classified a healthy thermostat coast as `compressor_failure` at 0.998).
Do not run cleanup or restart ai-agent in the last ~7 minutes before judging.

| t | Action | What judges see |
|---|---|---|
| 0:00 | Fleet grid all green (12 OK) | Live KPIs + Sites strip |
| 0:30 | Demo drawer → inject `power_outage` on FZ-006 | (warms — talk through the status ladder while the window fills) |
| 2:00 | Approve `compressor_failure` class (AI Control) | Guardrail policy editor, per-class approvals |
| ~3:30 | FZ-006 incident opens, `decision PROPOSED` | NEW class classified live (conf ≥ 0.88), topFactors `power_mean`/`compressor_duty`, click Explain → LLM RCA |
| 4:00 | Inject `compressor_failure` on FZ-002 | (approved class → AUTO path) |
| ~6:30 | FZ-002 `decision AUTO` → agent remediates | Command QUEUED→SENT→**ACKED (~12 ms)**, tile recovers, incident auto-resolves (`resolvedBy: system:recovery` after 60 s healthy) |
| 7:00 | Inject on FZ-004 + FZ-005 at once | (second simultaneous failure) |
| ~9:30 | Both incidents open | Fleet circuit breaker → **HOLD latched for the episode** (FZ-005 stays HOLD even after FZ-004 recovers) |
| 10:00 | "Open trace in Datadog" + Compliance CSV | `mqtt.ingest→mongo.write→ai.decision` span chain; HACCP excursion export closes the story |

## 5. Verified live evidence (this rehearsal)

- Fleet 12×OK baseline; inject → fault fired, ACKED latency ~12 ms
- `power_outage` classified live at conf 1.00 (the incident-open gate now
  follows the operator's `minConfidence` 0.85 instead of a fixed 0.55), with
  `power_mean` + `compressor_duty` as top factors
- AUTO path: FZ-002 compressor_failure conf 0.97 → `decision AUTO`, remediated
  by ai-agent, auto-resolved by `system:recovery` ~91 s later
- Circuit breaker: 2 simultaneous failures → FZ-004 AUTO + FZ-005 **HOLD**, and
  the HOLD stays latched after FZ-004 resolves (regression test pins it)
- No false re-open after recovery: the 0.58-confidence `power_outage` that used
  to open seconds after a resolve no longer clears the gate
- Model eval: **100% accuracy**, 1100 held-out windows, 5 classes, clean confusion matrix
- Compliance: the seeded historical excursion gives the HACCP CSV rows before
  any live fault has crossed −14 °C
- Mongo showcase: `$facet`/`$setWindowFields`/`$derivative`/`$lookup`/`$group` in one
  endpoint; incident memory by in-app cosine over 13-dim feature vectors
  (top-3, source device excluded) — no search index needed on the local cluster

## 6. talking points by sponsor

- **MongoDB**: replica set for change streams (SSE bus, no polling), time-series
  collection with minutes granularity, in-app cosine incident memory, JSON-schema validation
- **MongoDB tiering**: raw readings TTL out at 24 h, but `$merge` rollups keep
  hourly 7 d / daily 365 d — show the device page History chart
  (`GET /api/devices/{id}/history`): the chart is the ROLLOUPS, not raw data,
  with local-time labels ($dateToString, Asia/Ho_Chi_Minh). The HACCP CSV
  (`/api/compliance/excursions.csv`) renders its audit column in the same tz.
- **OneNexus**: the configured model RCA + streaming chat (catalog.onenexus.cloud/v1),
  deterministic template fallback if the API is down — demo never breaks
- **AI memory**: the "similar past incidents" panel is a real cosine search over
  RESOLVED incident feature vectors — seeded from the same simulator physics
  the classifier was warm-started on (tools/seedhistory), so train and serve
  distributions match by construction
- **Datadog APM/OTel**: one `ai.decision` span per AI decision with factor attributes;
  traceId stored on the incident doc deep-links UI → trace
