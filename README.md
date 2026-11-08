# ColdTrace

Cold-chain IoT monitoring POC for the MongoDB + Datadog hackathon.
Virtual freezer fleet -> MQTT -> Go gateway -> MongoDB Time Series,
observed end-to-end in **Datadog** (APM traces, metrics, logs, anomaly
monitors) via the OpenTelemetry Collector — no vendor SDK in the apps.

## Stack (current stable versions)

- Go 1.27 (simulator, gateway, tools)
- eclipse-mosquitto:2.1.2-alpine (MQTT broker)
- mongodb-driver v2 (go.mongodb.org/mongo-driver/v2)
- OpenTelemetry Go SDK (OTLP HTTP) — traces + metrics + **logs**, with
  trace-aware slog (log records carry trace_id/span_id)
- **otel-collector (contrib 0.164.0)** — the telemetry switchboard: apps
  export OTLP to it; it ships traces, metrics and logs to Datadog with the
  native `datadog` exporter + APM stats connector
- datadog/agent:7.83.3 (infra metrics, container logs, MongoDB fleet rollup)
- mongo:8.3.13-noble (single-node RS for change streams)
- node:24-slim / oven/bun:1.4.4 (ops-console build; Debian, not Alpine — the
  prebuilt @next/swc binary needs glibc)
- rollup retention: raw 24 h · hourly 7 d · daily 365 d

## Port map (host)

| Port  | Service                                  |
|-------|------------------------------------------|
| 8099  | ColdTrace gateway REST (container :8080) |
| 8091  | AI Ops agent REST                        |
| 8092  | Ops API (REST + SSE)                     |
| 3301  | Ops Console                              |
| 4317/4318 | otel-collector OTLP gRPC/HTTP        |
| 13133 | otel-collector health (readiness)        |
| 8125/8126 | Datadog Agent DogStatsD / APM        |
| 1883  | MQTT broker                              |
| 27017 | MongoDB                                  |
| —     | rollup (worker, no port; every 5 min)     |

## Telemetry routing

```
apps (gateway, ai-agent, ops-api, fleet-simulator)
  └─ OTLP HTTP → otel-collector (:4318)
       ├─ datadog exporter            → traces, metrics, logs
       └─ datadog connector           → APM stats (service map, latency)
datadog-agent (:8125/:8126)            → infra metrics, container logs,
                                          MongoDB custom-query fleet rollup
```

- All three signals flow: traces, metrics AND OTLP logs (slog bridge adds
  trace_id/span_id to every log record).
- **Writes are batched**: the gateway buffers readings and flushes with
  `insertMany` + `{ ordered: false }` (100 docs or 250 ms, whichever first)
  with bounded exponential-backoff retries — the MongoDB time series brief's
  recommended write path. Traces show one `mongo.write_batch` span per batch
  instead of one `mongo.write` per reading.
- **Hot/warm split**: raw readings live 24 h in the time series collection;
  `cmd/rollup` downsamples them into `telemetry_hourly` (7 d) and
  `telemetry_daily` (365 d) with `$group` → `$merge`, bucketed in
  `Asia/Ho_Chi_Minh`.
- Distributed traces span device → gateway → AI agent: the simulator mints
  a W3C `traceparent` per reading (payload field + MQTT user property),
  `mqtt.ingest` and `ai.decision` continue the same trace.
- **Datadog is the only backend.** Put `DD_API_KEY` (and `DD_SITE` for a
  non-US region) in `.env`; every service starts by default — no profile.
  The collector (`otelcol/config.yaml`) exports with the native `datadog`
  exporter + APM connector; the Agent ships infra metrics, container logs
  and the MongoDB custom-query fleet rollup
  (`mongodb.coldchain.fleet.device_count{status:ok|at_risk|alarm}`).
- Dashboard + monitors (`datadog/dashboard.json`, `datadog/monitors.json`)
  are pushed once via `./datadog/apply-datadog.sh`.

## Layout

```
broker/            mosquitto config
cmd/gateway/       ingest + REST + remediation API
cmd/simulator/     virtual freezer device (MQTT client)
internal/sim/      device physics incl. compressor_failure mode
internal/ingest/   MQTT -> Mongo + OTLP spans (mqtt.ingest, mongo.write)
internal/otlp/     OTLP trace/metric/log provider wiring (Datadog via collector)
internal/ai/       classifier, forecast, anomaly scorer, policy (+ ai.decision spans)
cmd/ai-agent/      AI Ops agent (change streams -> classify/predict/remediate)
internal/aggdb/    $setWindowFields/$derivative pipelines
cmd/rollup/        hourly+daily downsampling ($group -> $merge, tz-aware)
tools/initdb/      creates time series collection (TTL 24h), indexes
tools/evalmodel/   scores the classifier against fresh simulator windows
tools/slopeprofile/ measures healthy vs fault temperature slopes (AT_RISK threshold)
datadog/           agent conf + dashboard + monitors JSON + apply script
otelcol/config.yaml otel-collector → Datadog (native exporter + APM connector)
scripts/           inject incident, verify recovery, preflight, demo cleanup, runbook
```

## Solution docs

| Doc | Contents |
|---|---|
| [docs/solution-architecture.md](docs/solution-architecture.md) | container/data-flow diagrams, design decisions (D1–D10), aggregation showcase, failure modes |
| [docs/solution-design.md](docs/solution-design.md) | functional spec: domain model, incident doc schema, AI pipeline + guardrails, API contract, acceptance criteria |
| [scripts/demo_runbook.md](scripts/demo_runbook.md) | 5-minute judging script (rehearsed ×2, log inside) |
| [scripts/ai_demo.md](scripts/ai_demo.md) | AI demo walkthrough |
| [docs/coldtrace-deck.html](docs/coldtrace-deck.html) | submission deck (standalone, Vietnamese; `docs/coldtrace-deck.pdf` is the exported PDF) |

## Setup on a new machine

**Prerequisites:** Docker (with the compose v2 plugin), Go 1.27+, `jq`, `curl`.

```bash
git clone <repo> && cd coldtrace-code
cp .env.example .env          # then fill in DD_API_KEY and an LLM key
./scripts/setup_local.sh      # checks tooling, builds, starts, seeds
./scripts/demo_preflight.sh   # go/no-go: expect 12/12
```

`.env` is **gitignored on purpose** — it holds real API keys and never goes to
the remote. Every machine keeps its own copy; `cp .env.example .env` after a
clone and paste the keys in.

`setup_local.sh` is idempotent and never overwrites an existing `.env`. It
waits for MongoDB, initiates the single-node replica set (change streams need
it), and runs `initdb` + `seedhistory`. Use `--no-seed` to skip seeding.

### Keys you need

| Key | Required | Without it |
|---|---|---|
| `DD_API_KEY` | for observability | collector + Agent ship nowhere; the demo still runs |
| `DD_APP_KEY` | only for `datadog/apply-datadog.sh` | dashboards/monitors cannot be pushed |
| `OPENAI_API_KEY` (+ `OPENAI_BASE_URL`, `OPENAI_MODEL`) | for live RCA | RCA falls back to the deterministic template engine |
| `ANTHROPIC_API_KEY` | optional, wins over OpenAI | — |
| `CURSOR_API_KEY` | optional, wins over both | — |

### Manual equivalent

```bash
docker compose up --build -d
docker compose exec mongodb mongosh --eval \
  'rs.initiate({_id:"rs0",members:[{_id:0,host:"mongodb:27017"}]})'   # first run only
# The host MUST be "mongodb", not the container's random name: the default
# config records the runtime hostname and the set fails to re-elect on restart.
MONGO_URI=mongodb://localhost:27017 go run ./tools/initdb
MONGO_URI=mongodb://localhost:27017 go run ./tools/seedhistory
./datadog/apply-datadog.sh        # once: dashboard + monitors
```

Gateway endpoints (host :8099):

- `GET /devices/health` — fleet health via window functions
- `GET /incidents` — incident timeline
- `POST /devices/{id}/remediate` — issue compressor_restart over MQTT

AI agent endpoints (:8091): `GET /ai/incidents`, `POST /ai/approvals/{faultClass}`,
`POST /ai/feedback/{id}`.

## Demo (see scripts/demo_runbook.md)

```bash
scripts/demo_preflight.sh            # go/no-go gate before judges
scripts/inject_incident.sh FZ-003    # start compressor failure
scripts/verify_recovery.sh FZ-003    # after remediation, confirm recovery
```

Ops Console: http://localhost:3301 (fleet grid, incidents with AI explainability,
Model Health card, alert rules, compliance export). Ops-api: :8092 (`/api/health`
reports the active AI agent engine — OneNexus/OpenAI-compatible chat, or Cursor
cloud agent). AI agent chat: console ⌘K or `POST /api/ai/chat` — the model pulls
live fleet facts via read-only tools (get_fleet, get_device, list_incidents,
telemetry_stats) before answering. Incident and device pages deep-link to the
Datadog trace waterfall (`app.<DD_SITE>`).

## AI demo (see scripts/ai_demo.md)

Datadog APM shows the full story: services `coldtrace-gateway` +
`coldtrace-ai-agent` (spans `mqtt.ingest`, `mongo.write`, `ai.decision`),
the service map, metrics, and the agent's JSON decision logs.
