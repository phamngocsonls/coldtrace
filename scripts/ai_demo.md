# ColdTrace AI Ops Agent — Demo Addendum

The AI layer (`coldtrace-ai-agent`, service map node `coldtrace-ai-agent`) adds
Classification, Prediction, Anomaly Detection, Vector Memory and a hybrid
autonomy policy on top of the base incident demo.

## What the AI does (per device, every 30s, event-driven via MongoDB)

1. **Features** — 13 statistical features per device window (temp mean/std,
   slope °C/min, compressor duty, power, door events, range, slope accel,
   reported-vs-cabinet offset, drift lag, compressor reattain, trend lag, net
   trend), z-score normalized with a scaler shared by training and inference.
2. **Classification** — online multinomial logistic regression
   (`internal/ai/classifier.go`), warm-started on simulator physics
   (220 windows/class × 5 classes, varying seed, phase and window length),
   softmax confidence; auto-learns from every resolved
   incident (`POST /ai/feedback/{id}` → SGD steps → `ai_learned` log).
3. **Prediction** — EWMA-smoothed slope projection → `etaAlarmMin` = minutes
   until the -10°C spoilage threshold.
4. **Anomaly** — EWMA z-score anomaly score per device (0..1).
5. **Incident memory (local cosine)** — resolved incidents store their feature
   vector; new incidents get "top-3 similar past incidents" by cosine similarity
   (`ai.Similarity` + `TopSimilar`), computed in the agent over ≤200 docs and
   excluding the source device itself. No search index and no Atlas cluster are
   involved (`ai.IncidentMemory`).
6. **Hybrid autonomy** (`ai.Evaluate`) —
   - class not yet approved → incident `PROPOSED` (AI proposes, human decides)
   - `POST /ai/approvals/{faultClass}` → class enters the allowlist
   - approved + confidence ≥0.85 + ETA ≤10min + cooldown OK + fleet
     circuit-breaker OK → **`AUTO`: agent publishes `compressor_restart` over
     MQTT itself**, records `remediatedBy: ai-agent`
   - guardrail failures → `HOLD` with explicit reason

## Verified live run (this session)

| Step | Result |
|---|---|
| Inject FZ-003 | `ai_decision`: `compressor_failure`, conf=1.0, `PROPOSED` |
| Approve class | `POST /ai/approvals/compressor_failure` → allowlist |
| Inject FZ-004 | classified `compressor_failure` conf=0.98→1.0 |
| First pass | `HOLD` — "time-to-alarm beyond horizon" (eta 34min > 10min gate) |
| Fault evolves, eta crosses gate | `ai_decision_refreshed`: `AUTO` → agent remediates itself |
| Recovery | FZ-004: ALARM → AT_RISK → **OK** in 5 minutes, no human click |
| Audit | `ai_incidents.remediatedBy = "ai-agent"`, approvals collection records who/when |

## Demo script (AI part, ~3 min)

1. Reset state (optional): drop `ai_incidents`/`ai_approvals` in mongosh.
2. `scripts/inject_incident.sh FZ-003` — wait ~1 min → show
   `GET :8091/ai/incidents`: `PROPOSED` + reasoning + similar-count.
3. `curl -X POST :8091/ai/approvals/compressor_failure` — "human approves once".
4. `scripts/inject_incident.sh FZ-004` — wait → first `HOLD` (eta gate),
   then `ai_decision_refreshed` → `AUTO` → agent remediates; FZ-004 recovers
   in ~5 min with zero human interaction.
5. Show the audit trail: `remediatedBy: ai-agent` + the guardrail reasons in
   each decision — this is the "AI you can trust" narrative for judges.

## AI Observability (Datadog APM + Logs — app.<DD_SITE>)

- Service `coldtrace-ai-agent` appears as its own node (OTLP traces/metrics);
  every policy outcome emits an `ai.decision` span carrying `ai.device`,
  `ai.fault_class`, `ai.decision`, confidence, ETA and all guardrail-gate
  attributes (`internal/ai/trace.go`).
- Decision logs: `ai_decision`, `ai_decision_refreshed`, `ai_learned`,
  `ai_class_approved`, `ai_warm_start` — full JSON with confidence, ETA,
  guardrail reasons (queryable in Datadog Logs, filtered by `service:coldtrace-ai-agent`).
- Gateway spans `mqtt.ingest` -> `mongo.write` under service
  `coldtrace-gateway`, visible in the Datadog service map and trace waterfall.
- Collections: `ai_incidents` (decisions + vectors + audit),
  `ai_approvals` (who approved what, when).

## MongoDB feature checklist this covers

- Change Streams: `ai_incidents` watched live (plain collection; note: TS
  collections are views — `CommandNotSupportedOnView` — telemetry windows are
  fed by cursor polling, documented in `cmd/ai-agent/main.go`).
- Event-driven processing: the 30s evaluator loop keyed off streamed data.
- Incident memory: in-app cosine over 13-dim feature vectors (no search index).
- Aggregations/window functions: unchanged from base demo (`$setWindowFields`,
  `$derivative`) — the AI consumes the same raw telemetry collection.
