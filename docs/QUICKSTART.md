# ColdTrace — Quickstart & Reading Guide

**You have 20 minutes and need to understand this product, run it, and be
ready to demo it. This is the only page you need first.**

---

## 1. What is ColdTrace? (60 seconds)

ColdTrace is an **AI ops agent for a cold-chain freezer fleet**. 12 virtual
freezers (each holding ~$12k of seafood) publish telemetry every 5 s:

```
freezer ──MQTT──▶ gateway ──▶ MongoDB time series ──▶ rollups (hourly/daily)
                     │                │
                  Datadog      ai-agent watches every window
                                 (classify → explain → decide → act → recover → learn)
```

When a compressor dies at 02:00, ColdTrace **classifies the fault in ~3
minutes** (`compressor_failure`, `door_left_open`, `sensor_drift`,
`power_outage`), **explains it** (top factors + LLM root-cause), **decides
within guardrails** (approved class? confidence ≥ 0.85? ETA ≤ 45 min? fleet
breaker not tripped?), **remediates by itself** (`compressor_restart` over
MQTT, device ACKs in ~12 ms), **proves recovery** (auto-resolves the incident
after 60 s of healthy telemetry), and **learns** from operator feedback —
with the whole chain visible as one Datadog trace.

**The one-sentence pitch:** *the AI operator that catches a dead freezer at
02:00, fixes it inside guardrails, proves it recovered, and files the HACCP
paperwork.*

---

## 2. Setup (3 commands)

```bash
cp .env.example .env            # then paste your keys (Mongo optional; DD_API_KEY for Datadog)
docker compose up -d --wait     # 9 services: broker, mongo RS, gateway, rollup, ai-agent, ops-api, console, collector, agent

docker run --rm --network coldtrace-code_default -v "$PWD":/src -v coldtrace-gomod:/go/pkg/mod -w /src \
  golang:1.27 sh -c 'MONGO_URI=mongodb://mongodb:27017 go run ./tools/initdb && MONGO_URI=mongodb://mongodb:27017 go run ./tools/seedhistory'
```

`initdb` creates the collections/indexes/12 devices. `seedhistory` plants demo
history: 24 resolved incidents (AI memory), 6 d hourly + 21 d daily rollups,
and a historical excursion so the compliance CSV has rows on day one.

Verify — **all 12 checks must pass**:

```bash
./scripts/demo_preflight.sh
```

Open the console: **http://localhost:3301**

> Host without Go? The `docker run` wrapper above is how everything Go runs
> here (tests too — see §7). The stack itself is pure `docker compose`.

---

## 3. Read in this order (total ~35 min)

| # | Doc | Time | What you get |
|---|---|---|---|
| 1 | **This page** | 5 min | Product story, setup, tour, cheat sheet |
| 2 | `docs/solution-design.md` | 15 min | The thinking: status ladder, the AI layer (13 features, warm start, guardrails), Mongo showcase queries, the incident document |
| 3 | `docs/mongodb-data-model.md` | 10 min | **Every collection**: document shape, indexes + why, the time-series/tiering design, validator, query→index mapping |
| 4 | `scripts/demo_runbook.md` | 5 min | Bring-up, pre-flight, the 10-minute stage script with **measured** timings |
| 5 | `docs/demo-video-plan.md` | 5 min | The **10-scene / 10-minute** video storyboard, what to say, how to hide detection waits, fallbacks |
| 6 | `docs/solution-architecture.md` | skim | Request flows + Mongo/Datadog mapping per endpoint (reference, not linear reading) |

If you only read two: this page + the **video plan §4 (what the AI does)** —
that table is the product knowledge you need on camera. If a judge asks
"show me your data model": open `docs/mongodb-data-model.md` and scroll.

---

## 4. Console tour (what to show, what each page proves)

| Page | What it is | The demo line |
|---|---|---|
| `/` Fleet Overview | 12 tiles, KPIs, Sites rollup, live event feed | "Status is COMPUTED (STALE→ALARM→AT_RISK→OK with deadbands), streamed over change streams — no polling" |
| `/devices/FZ-002` | uPlot live temp chart, device twin, controls, **History chart = the rollup tiers** | "Raw readings TTL out at 24 h; `$merge` rollups keep hourly 7 d / daily 365 d — this chart reads the ROLLOUPS, local-time labeled" |
| `/incidents` | AI decision cards: topFactors bars, guardrail gates, LLM RCA, similar-incidents memory, trace link | "The AI shows its work: top-3 feature contributions, an LLM root-cause grounded in real telemetry, and 3 cosine-similar past incidents" |
| `/ai` AI Control | Per-class approvals, guardrail policy editor, kill switch, **Model Health** (confusion matrix) | "Autonomy is a dial the operator owns: approve a class, set confidence/ETA/cooldown, or kill all actions instantly" |
| `/events` | Filterable audit trail + operator alert rules | "Every decision, command and ack is one audit trail" |
| `/compliance` | **HACCP excursion CSV export** | "Minutes above −14 °C from actual sample deltas, last excursion in the auditor's timezone — this goes in the binder" |

---

## 5. Demo cheat sheet (copy-paste)

```bash
# Break things (pick any: compressor_failure door_left_open sensor_drift power_outage)
curl -s -X POST localhost:8092/api/demo/inject -H 'Content-Type: application/json' -d '{"deviceId":"FZ-002","fault":"compressor_failure"}'

# Approve a class for AUTO-remediation (or use /ai in the console)
curl -s -X PUT localhost:8092/api/approvals/compressor_failure

# Reset the whole fleet to pristine (faults cleared, cabinets at -18, AI windows reset)
curl -s -X POST localhost:8092/api/demo/reset

# Rehearsal cleanup + preflight (ALWAYS before judging/recording)
./scripts/demo_cleanup.sh && ./scripts/demo_preflight.sh
```

**The two rules that save every demo:**
1. **Wait ~7 min after reset/restart before injecting** — the classifier needs
   a warm 6-min window (72 samples @ 5 s). Detection lands ~2.5–4 min after
   inject when warm.
2. **Never run cleanup or reset mid-scenario** — it resolves all OPEN
   incidents and clears the AI windows, which will confuse the story.

Current demo state on this machine: `compressor_failure` approved (AUTO
path), policy `minConfidence 0.85 / ETA 45 / breaker 2 / cooldown 30 min`,
fleet 12×OK, 24 seeded resolved incidents, compliance CSV seeded.

---

## 6. Key concepts (the 5 you'll be asked about)

1. **Status ladder** — `STALE → ALARM (>−10 °C) → AT_RISK (>−14 °C, or >setpoint+2 °C, or compressor off above deadband) → OK`. Never stored; computed on read. Deadbands matter: a resting compressor at −17.9 is NOT a fault.
2. **The AI loop** — detect (softmax over 13 features) → explain (topFactors + LLM RCA) → remember (cosine over past incident vectors) → decide (guardrails) → act (MQTT command) → **recover (60 s healthy → auto-resolve `system:recovery`)** → learn (feedback → `Learn()`).
3. **Guardrails** — class approval, confidence ≥ 0.85 (the same gate that decides whether an incident opens AT ALL), ETA ≤ 45 min, per-device 30-min cooldown, fleet circuit breaker (≥ 2 devices ALARM → HOLD, **latched for the episode**), kill switch.
4. **Tiering** — raw 24 h TTL → `$merge` rollups hourly 7 d / daily 365 d, bucketed in `Asia/Ho_Chi_Minh`. The History chart reads rollups; the compliance export reads raw.
5. **Traceability** — every reading starts a W3C trace; `mqtt.ingest → mongo.write_batch → ai.decision` is one span chain; the incident card deep-links into Datadog APM and logs correlate by trace_id.

---

## 7. Tests & tools

```bash
# All tests (in the Go container — the host has no Go):
docker run --rm --network coldtrace-code_default -e MONGO_URI=mongodb://mongodb:27017 \
  -v "$PWD":/src -v coldtrace-gomod:/go/pkg/mod -w /src golang:1.27 go test ./...

# Model accuracy report (also at GET /api/model/eval):
#   100% on 1,100 fresh simulator windows, 5 classes — measured, not stored

# Refresh demo history (idempotent, seeded:true only):
docker run --rm --network coldtrace-code_default -v "$PWD":/src -v coldtrace-gomod:/go/pkg/mod -w /src \
  golang:1.27 go run ./tools/seedhistory
```

---

## 8. Troubleshooting (the 3 that actually happen)

| Symptom | Fix |
|---|---|
| Detection > 5 min after inject | Cold AI windows. `POST /api/demo/reset`, wait a full 7 min, re-inject |
| Fleet tiles stuck / not updating | `docker compose restart ai-agent` — its Mongo poll loops don't survive a broker restart (runbook §2) |
| Console 200 but charts empty | `demo_preflight.sh` check 7 pins it (series pipeline); usually means `initdb` wasn't run |

Everything else: `./scripts/demo_preflight.sh` prints the exact fix per check.
