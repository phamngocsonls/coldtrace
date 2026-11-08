# ColdTrace — MongoDB Data Model

The complete schema reference for the `coldtrace` database: every collection,
its document shape, its indexes and **why each one exists**, the time-series
and tiering design, and the query-pattern → index mapping judges ask about.
The living definition of this doc is `tools/initdb/main.go` (+ the Go structs
named per collection); this page must never drift from them.

Related reading: `docs/solution-design.md` §3 (the narrative data model) and
§4 (the AI layer that reads/writes `ai_incidents`), `docs/solution-architecture.md`
(request flows per endpoint).

---

## 1. Ownership (single-writer rule)

```
gateway      owns  telemetry (TS)  ·  device_state.reported
ai-agent     owns  ai_incidents    ·  commands (auto actions)
ops-api      owns  events          ·  alert_rules · ai_policy · ai_approvals
             (operator actions)     + device_state.desired (setpoint overrides)
initdb       owns  devices (registry) — seeded once, read-only at runtime
rollup       owns  telemetry_hourly · telemetry_daily ($merge upserts)
```

One writer per field means change streams are unambiguous (the SSE bus
replays them) and no service needs a locking protocol. The one deliberate
exception: `device_state` is a twin — gateway writes `reported`, ops-api
writes `desired` — two disjoint subdocuments on one doc.

All writes are UTC (`time.Time` → BSON date); presentation tz
(`Asia/Ho_Chi_Minh`) happens only in `$dateToString` projections, never in
storage or comparison.

---

## 2. The time series: `telemetry`

The only time-series collection — everything hot flows through it.

```js
// createCollection (initdb):
{ type: "timeseries", timeField: "ts", metaField: "meta",
  granularity: "minutes", expireAfterSeconds: 86400 }   // 24 h TTL
```

One reading:

```jsonc
{
  "ts":   "2026-11-08T07:20:05Z",       // timeField
  "meta": { "deviceId": "FZ-003",       // metaField: FIXED labels only
            "site": "OM-DNG-01",        //   (rarely change, filter on them)
            "zone": "A2" },
  "tempC": -18.03,                      // measurements (the rest)
  "powerDraw": 402.7,
  "compressorRunning": true,
  "doorOpen": false,
  "seq": 618
}
```

Design decisions (all live in code, each with its failure story):

- **metaField = fixed labels only.** Per the time-series brief, `meta` holds
  the source id plus labels you *filter* on and that *rarely change* — never a
  measured value. All readings with equal `meta` share a bucket, so
  `{"meta.site": "OM-DNG-01"}` skips whole buckets instead of scanning. The
  `metaField` is **immutable after creation**; `initdb` fails loudly if a
  legacy collection has the wrong one rather than silently missing every
  `meta.*` filter.
- **`granularity: "minutes"`** matches the 5 s publish cadence (readings
  arrive well within a 60 s bucket span).
- **TTL 24 h** (`expireAfterSeconds: 86400`): raw readings are the hot tier —
  fine-grained windows need them, auditors don't after a day. `initdb` guards
  against a plain collection (no TTL, unbounded scans) — it hard-fails with
  the drop+recreate command instead of limping.
- **Batched writes**: the gateway buffers and flushes with
  `insertMany` + `{ordered: false}` (100 docs or 250 ms, bounded exponential
  backoff) — the brief's recommended write path. APM shows one
  `mongo.write_batch` span per flush, not one write per reading.
- **Indexes** (time series accepts secondary indexes; both are the
  filter-then-time pattern from the perf tips):
  - `{meta.deviceId: 1, ts: -1}` — every device page query
    (`$match deviceId` + window)
  - `{meta: 1, ts: 1}` — site/zone-wide scans (`insights`, fleet rollups)

---

## 3. Tiering: `telemetry_hourly` / `telemetry_daily`

Ordinary collections written by `cmd/rollup` every 5 min:

```
$match (last 3h) → $setWindowFields partition by meta.deviceId
  → $group by $dateTrunc(Asia/Ho_Chi_Minh, hour|day)
  → $merge(on: [deviceId, bucket], whenMatched: replace, whenNotMatched: insert)
```

| Field | Type | Notes |
|---|---|---|
| `deviceId` | string | partition key |
| `bucket` | date | **local** hour/day boundary via `$dateTrunc` in ICT |
| `unit` | `"hour" \| "day"` | tier discriminator |
| `avgTempC` `minTempC` `maxTempC` | double | the chart bands |
| `avgPowerDraw` | double | |
| `compressorDuty` | double | compressor-on fraction |
| `doorEvents` | int | door-open count in bucket |
| `readings` | int | sample count (720/hour at 5 s) |

- **Unique index `{deviceId: 1, bucket: 1}`** (`uniq_deviceId_bucket`) is
  *required by `$merge`* — its `on` fields must be backed by exactly this
  uniqueness. `initdb` creates it so the rollup works on a fresh DB.
- Retention: hourly 7 d (1 728 buckets live), daily 365 d (12 × 364).
  Enforced by `seedhistory`/`rollup` window math (bucket-aligned deletes),
  not TTL — the tiers are summaries, not event data.
- `tools/seedhistory` pre-plants both tiers (`seeded: true`) strictly
  **before the current hour** so nothing collides with the rollup's live
  `$merge` window; the tier reader `$project`s it away, so seeded and live
  rows are indistinguishable on the chart.
- Read path: `GET /api/devices/{id}/history?tier=…` — `$match` on
  deviceId+range first, then tz-aware `$dateToString` for the
  bucket-local labels the chart shows.

---

## 4. Collections reference

### `devices` — the registry (initdb-owned, $jsonSchema-validated)

The only collection with a **validator** — the registry is the contract
every other service joins against (meta labels, fleet `$lookup`, value-at-risk
KPIs), so a malformed device must be rejected at write time:

```jsonc
{ "$jsonSchema": {
    "bsonType": "object",
    "required": ["deviceId","site","zone","product","valueAtRisk","setpoint"],
    "properties": {
      "deviceId":  { "bsonType": "string", "pattern": "^[A-Z]{2}-\\d{3}$" },
      "site":      { "bsonType": "string" },
      "zone":      { "bsonType": "string" },
      "product":   { "bsonType": "string" },
      "valueAtRisk": { "bsonType": "int", "minimum": 0 },
      "setpoint":  { "bsonType": "double", "minimum": -30, "maximum": 0 } } } }
```

Document: `deviceId, site, zone, product, valueAtRisk (USD), setpoint (-18),
alarmAboveC (-10), atRiskAboveC (-14), model, commissionedAt`.
Unique index `{deviceId: 1}`. Seeded with the 12-freezer demo fleet (OM-DNG-01
premium beef/seafood/ice cream · OM-DNG-02 beef/ice cream · OM-DNG-03
seafood/**vaccines**/meals — FZ-010 at $25k is the stress-test VAR number).

### `device_state` — the digital twin (gateway + ops-api)

One doc per device, the SSE bus's main source. Two disjoint writer halves:

```jsonc
{
  "deviceId": "FZ-003",
  "reported": {                // gateway — from every MQTT reading
    "tempC": -18.03, "powerDraw": 402.7, "compressorRunning": true,
    "doorOpen": false, "seq": 618, "ts": "2026-11-08T07:20:05Z" },
  "desired": {                 // ops-api — operator intent (SSE + AI read it)
    "setpoint": -18, "updatedAt": "…", "updatedBy": "operator:demo" },
  "lastSeen": "2026-11-08T07:20:05Z"
}
```

Unique index `{deviceId: 1}`. `initdb` seeds `desired.setpoint` from the
registry (`$ifNull` keeps operator overrides intact) — without it a fresh
device has nothing for the fleet status ladder to compare against.

### `ai_incidents` — the AI audit trail (ai-agent)

The centerpiece document — full annotated example in
`docs/solution-design.md` §3.1. Fields: `_id, deviceId, faultClass,
confidence, etaAlarmMin (nil when no rising path — never NaN),
features (13-dim normalized), decision (PROPOSED|AUTO|HOLD), reason,
similar[] (deviceId/faultClass/score/openedAt — camelCase BSON keys),
circuitBreaker (latched fleet-breaker flag), status (OPEN|RESOLVED),
remediatedAt/remediatedBy, resolvedAt/resolvedBy, openedAt, traceId,
topFactors[], rca{}, acknowledged, seeded (history seeds only)`.

Indexes: `{openedAt: -1}` (the incident list is newest-first) and
`{deviceId: 1, faultClass: 1, status: 1}` — the dedup lookup ("one OPEN episode
per device") *and* the fleet circuit-breaker count both hit it.
`ai_policy.maxEtaAlarmMinutes` etc. live in `ai_policy`, not here.

### `commands` — the action lifecycle (ops-api / ai-agent)

```jsonc
{ "commandId": "6af01b26…", "deviceId": "FZ-002",       // or "FLEET" for broadcasts
  "action": "compressor_restart|set_fault|set_setpoint|defrost|reboot|demo_reset",
  "args": {}, "status": "QUEUED|SENT|ACKED|FAILED|TIMEOUT",
  "issuedBy": "ai-agent|operator:<name>", "latencyMs": 12,
  "ackDetail": "fault cleared, device back online",
  "createdAt": "…", "sentAt": "…", "ackedAt": "…", "expiresAt": "…" }
```

Indexes: unique `{commandId}`, `{deviceId, createdAt}` (per-device history),
`{status, expiresAt}` (the sweeper that marks stale SENT → TIMEOUT).
The QUEUED→SENT→ACKED lifecycle is the audit story: auto-remediation and
operator actions leave identical trails.

### `events` — the unified audit log (all services)

`type (device_fault|command_*|ai_*|approval|operator|recovery|demo_*),
deviceId?, message, actor (ai-agent|operator:*|system|device), commandId?,
incidentId?, severity? (info|warning|critical), data?, at, expiresAt`.

Indexes: `{at: -1}` (feed), `{type, at}`, `{deviceId, at}` (filterable pages),
**TTL index on `{expiresAt: 1}`** — the only value-driven expiry: the feed
keeps 7 d (`expiresAt = at + 7d` set by the writer), audit data lives in
`ai_incidents`/`commands`, not here.

### `ai_policy` — the guardrail dial (ops-api)

One doc: `{minConfidence: 0.85, maxEtaAlarmMinutes: 45, autoCooldownMinutes: 30,
fleetCircuitBreaker: 2, killSwitch: false, updatedAt, updatedBy}` — re-read by
the agent every cycle (≤30 s cache), so turning the kill switch on takes effect
without a redeploy. `initdb` seeds defaults if empty.

### `ai_approvals` — per-class autonomy grants (ops-api)

`{faultClass, approvedAt, approvedBy}` — unique index on `{faultClass}` keeps
it one doc per class (upsert semantics). "Approve compressor_failure" is
literally one upsert into this collection.

### `alert_rules` — operator thresholds (ops-api)

`{ruleId (unique), deviceId? ("" = fleet-wide), metric (reported.tempC |
reported.powerDraw | reported.compressorRunning | lastSeenAgeSec),
op (gt|lt|eq), threshold, severity, enabled, createdBy, createdAt}` —
evaluated by the ops-api 30 s rule engine against the twins.
Index `{deviceId, createdAt}` for per-device rule lists.

### `incidents` — legacy gateway timeline

Kept for compatibility (`{deviceId, openedAt}` index); the AI pipeline writes
`ai_incidents`. Do not build on it.

---

## 5. Query patterns → index mapping (the judge question)

| Pattern | Where | Pipeline shape | Index it lands on |
|---|---|---|---|
| Fleet status ladder | `GET /api/fleet` | `$lookup` twin → `$lookup` devices → `$switch` statusExpr → `$group` by site | `device_state.deviceId`, `devices.deviceId` |
| Health slope view | `GET /devices/health/:id` | `$match` → `$setWindowFields` + `$derivative` (5-min slope) | `telemetry {meta.deviceId, ts}` |
| Live chart series | `GET /api/devices/:id/series` | `$match` first → `$setWindowFields` fill | same |
| HACCP excursion audit | `GET /api/compliance/excursions.csv` | `$match tempC > -14` → `$setWindowFields/$shift` span credit → `$group` → **`$dateToString` with explicit tz** | `telemetry {meta.deviceId, ts}` |
| Tier history chart | `GET /api/devices/:id/history` | `$match` → `$dateToString` (tz labels) | `telemetry_* {deviceId, bucket}` |
| Incident dedup + breaker count | ai-agent evaluate | `FindOne {deviceId, status: OPEN}` / `CountDocuments {status, faultClass, deviceId ≠ self}` | `ai_incidents {deviceId, faultClass, status}` |
| Incident memory | ai-agent similarPast | `Find {status: RESOLVED} → in-process cosine` | `{openedAt}` (bounded scan, 200) |
| SSE bus | ops-api `/api/stream` | change streams on `device_state`, `events`, `ai_incidents` | oplog (replica set required — hence RS, not standalone) |
| Command sweep | ops-api every 30 s | `{status: SENT, expiresAt < now}` | `commands {status, expiresAt}` |

Two shapes to call out on camera: **`$match` is always first** (index-narrow
before any window work), and **every date that renders for a human carries an
explicit timezone** — the audit CSV's "last excursion" column and the tier
chart's bucket labels both `$dateToString` in `Asia/Ho_Chi_Minh`, while all
math/comparison stays UTC.

A third, learned the hard way: **every window pipeline deduplicates on
`(deviceId, ts)` first** (`aggdb.DedupByTS()` — `$group`/`$replaceRoot`/`$sort`).
MQTT QoS 1 is at-least-once, one broker redelivery writes the same reading
twice, and `$fill`/`$setWindowFields` REQUIRE a unique sortBy per partition —
a single duplicated tick 500'd the series chart, the health view and the
audit until the dedup absorbed it. Time-series collections cannot take unique
indexes, so the read path owns idempotency (regression test:
`aggdb.TestDedupByTSFixesWindowPipelines`).

---

## 6. Ops notes

- **Re-init is safe**: `initdb` is idempotent (`NamespaceExists` → "exists
  (kept)") and *hard-fails* on the two unrecoverable misconfigurations
  (plain `telemetry` collection, wrong `metaField`) with the exact fix
  printed.
- **Re-seed is safe**: everything `seedhistory` writes carries
  `seeded: true`; it deletes exactly its own prior set (`ReplaceFilter`)
  and range-deletes its tier windows before inserting — never live agent
  incidents, never rollup-written buckets.
- **Reseed the demo history** any time:
  `docker run … golang:1.27 go run ./tools/seedhistory` (see QUICKSTART §7).
- **Schema drift**: this page's source of truth is `tools/initdb/main.go` +
  the per-service structs (`ops.Command`, `ops.Event`, `ops.Rule`,
  `ai.SimilarMatch`, `seed.Summary`, `seed.Incident`). Change those, run
  `initdb`, then update this page in the same commit.
