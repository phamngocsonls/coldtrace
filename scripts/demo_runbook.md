# ColdTrace Demo Runbook

## 1. Bring-up (one command, ~2 min)

```bash
docker compose up -d --build --wait          # full stack incl. Datadog (9 services)
docker compose --profile loadgen up -d       # optional load generator
MONGO_URI=mongodb://localhost:27017 go run ./tools/initdb  # collections, indexes, devices
MONGO_URI=mongodb://localhost:27017 go run ./tools/seedhistory     # demo history: resolved
                                                                   # incidents (AI incident memory)
                                                                   # + 6d hourly / 21d daily tiers
```

Then push the Datadog dashboard + monitors (one-time; needs both keys in `.env`):

```bash
./datadog/apply-datadog.sh
# judges: app.<DD_SITE> → "ColdTrace — Cold-chain Fleet Health" dashboard
```

## 2. Pre-demo checks (5 min before judging)

```bash
./scripts/demo_preflight.sh   # all 11 checks must pass; exits non-zero otherwise
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

## 4. Ops Console — 5-minute winning script

| t | Action | What judges see |
|---|---|---|
| 0:00 | Fleet grid all green (12 OK) | Live KPIs + Sites strip |
| 0:30 | Demo drawer → inject `power_outage` on FZ-006 | NEW class classified live (conf ≥0.88), topFactors show `compressor_duty`/`power_mean` driving the call |
| 1:30 | Click Explain → LLM RCA | the configured model RCA grounded in twin + window stats |
| 2:00 | Approve `compressor_failure` class (AI Control) | Guardrail checklist per decision |
| 2:30 | Inject `compressor_failure` on FZ-002 → agent AUTO-remediates | Command QUEUED→SENT→**ACKED (~13 ms)**, tile recovers, incident auto-resolves (`resolvedBy: system:recovery` after 60s healthy) |
| 3:30 | Inject on 2 devices at once | Fleet circuit breaker → HOLD with reason (guardrail holds) |
| 4:00 | "Open trace in Datadog" from the incident | `mqtt.ingest→mongo.write→ai.decision` span chain with `ai.top_factor.*` attributes |
| 4:30 | Resolve + feedback → model learns | Compliance export + VAR/MTTR KPIs to close |

## 5. Verified live evidence (this rehearsal)

- Fleet 12×OK baseline; inject → fault fired, ACKED latency ~13 ms
- `power_outage` classified live (conf 0.84–0.94), `power_mean` + `compressor_duty` top factors
- AUTO path: FZ-002 compressor_failure → `decision AUTO`, remediated by ai-agent,
  twin recovered to −18.4°C, incident auto-resolved on sustained healthy window
- Circuit breaker: 2 ALARM devices → HOLD "too many devices in ALARM"
- Model eval: **100% accuracy**, 1100 held-out windows, 5 classes, clean confusion matrix
- Mongo showcase: `$facet`/`$setWindowFields`/`$derivative`/`$lookup`/`$group` in one
  endpoint; incident memory by in-app cosine over 8-dim feature vectors
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
