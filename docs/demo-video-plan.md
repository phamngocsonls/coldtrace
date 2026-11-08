# ColdTrace — Demo Video Plan

A complete production plan for a **10-minute product demo video**: what to
record, in which order, what to say, and how to hide the boring parts. Every
timing below was **measured live** on this stack (not estimated), and every
scene has a fallback so a single hiccup cannot sink the recording.

---

## 0. One-paragraph story (the pitch the video must land)

> 12 freezers hold $250k of seafood. A compressor dies at 02:00. The box
> crosses −14 °C at 02:40 and −10 °C — the spoilage line — at 03:20.
> Nobody is watching at 02:00.
>
> ColdTrace is the AI operator that is: it classifies the fault from the
> thermal pattern in **~3 minutes**, explains **why** (top factors + LLM
> root-cause), remembers **similar incidents**, decides **within guardrails**
> (approved class, confidence, ETA, fleet circuit breaker), remediates by
> itself (**command → device ACK in 0–38 ms**), then **auto-resolves** the
> incident when the cabinet is provably healthy again — and the whole chain
> is one trace in Datadog.
>
> And when it **can't** fix it — no remote action, or the device refuses —
> it says so and **pages a human**, pre-drafted. Three resolution paths
> (auto / on-call / escalate), one audit trail.

**The resolution arc is the spine of the video.** Every failure ends in one
of three actions, and each is demonstrated: **A** auto-restart, **B** human
dispatch, **C** breaker escalation, plus **A′** the agent failing safely.

---

## 1. Recording setup

| Item | Recommendation |
|---|---|
| Tool | OBS Studio (or Screen Studio for auto-zoom) |
| Window | Browser at **1512×982**, macOS Chrome, zoom 100% |
| Area | Console `http://localhost:3301` + one terminal pane for `curl` injects |
| Cursor | Enable "click highlight" (OBS: Cursor Spotlight effect) |
| Audio | Record narration **after** the screen, per scene — easier to re-take |
| Theme | Use the console dark theme as recorded — it is the product look |
| Capture | 60 fps if panning between pages; 30 fps acceptable |

**Two-terminal layout works well:** left = console, right = a terminal where
you run the `curl` inject commands, so viewers SEE the action → reaction.

---

## 2. Pre-record checklist (T-30 min)

**Learn the product first (if you haven't):** `docs/QUICKSTART.md` §1–§6 —
the 60-second pitch, the console tour table, the demo cheat sheet and the 5
key concepts. That page plus this plan is everything you need on camera;
`solution-design.md` is the deep reference if a judge drills in.

Run in order; every line is verified by `scripts/demo_preflight.sh`.

```bash
docker compose up -d --wait                                # stack healthy
MONGO_URI=mongodb://localhost:27017 go run ./tools/initdb   # indexes exist
MONGO_URI=mongodb://localhost:27017 go run ./tools/seedhistory
#   → 24 resolved incidents (AI memory) + 6d hourly + 21d daily tiers
#   + a 22-min raw excursion on FZ-009 → the HACCP CSV has rows on day one
./scripts/demo_cleanup.sh                                  # 12×OK, zero OPEN
./scripts/demo_preflight.sh                                # 12/12 must pass
```

Then the **critical timing rule** (this is the #1 way a recording goes wrong):

> **Wait ~7 minutes after cleanup before the first inject.** The classifier
> needs a warm ~6-minute window (72 samples @ 5 s) before it can be confident.
> Detection lands **~2.5–4 min after inject** when warm, **~6.5 min** when
> cold. Do not run cleanup, restart `ai-agent`, or call `/api/demo/reset` in
> the last 7 minutes before a scene.

State to confirm before recording:

- `GET /api/approvals` → **only `compressor_failure` approved** (the video
  needs `power_outage` to be PROPOSED first, then show the approval moment)
- `GET /api/policy` → `minConfidence 0.85`, `maxEtaAlarmMinutes 45`,
  `fleetCircuitBreaker 2`, kill switch off
- Console open on Fleet Overview, SSE live (tiles flip every 5 s)
- Datadog logged in on `app.ap1.datadoghq.com` (traces + logs)

---

## 3. Storyboard (scene by scene)

Total runtime target: **10:00** (eleven scenes). Real detection waits are hidden
with **cuts** (see §5), so wall-clock recording stays ~40 min for the 10 min
video. The spine is **resolution**: three action paths (AUTO / on-call /
escalate) plus A′ (the agent failing safely), all shown.

### Scene 1 — The problem (0:00–0:40) *no live stack needed*

- **Show:** `docs/screenshots` or the deck's opening: freezer warms −18 →
  −14 → −10 while nobody watches. One stat on screen: *"FZ-010 holds $25k of
  vaccines — the spoilage line is −10 °C."*
- **Say:** the pitch from §0. End on: *"This is ColdTrace. Let me kill a
  compressor and watch it handle it."*
- Why first: judges decide in the first 30 s whether the rest is worth
  watching. A dead freezer is the whole business case.

### Scene 2 — Fleet Overview + the status ladder (0:40–1:30)

- **Show:** console `/` — 12 green tiles, KPI strip, Sites rollup, the live
  event feed ticking (SSE, not polling).
- **Say:** *"Twelve freezers, three sites, all green. Status is COMPUTED
  never stored — STALE → ALARM → AT_RISK → OK, with a deadband so a resting
  compressor doesn't paint the fleet yellow. It's live over MongoDB change
  streams — no refresh, no polling."*
- **Proof moment:** point at a tile's temperature updating every 5 s.

### Scene 3 — MongoDB under the hood (1:30–2:20) ⭐ sponsor scene

- **Show:** device detail page → the **History chart**, then flip the tier
  switch **hourly → daily** and let the chart re-render (7 d of hourly
  buckets, then 3 weeks of daily). Optional split-screen with
  `docs/mongodb-data-model.md` §1 (the ownership diagram).
- **Say (the Mongo story in 50 s):** *"Every reading lands in a MongoDB
  time-series collection — metaField holds the device labels so a site
  filter skips whole buckets. Raw readings TTL out at 24 h, but `$merge`
  rollups keep hourly for 7 days and daily for a year — that chart you just
  saw flip is reading the ROLLOUPS, bucketed in local time, not raw data.
  The live grid is change streams: one oplog tail, no polling. And the
  registry is `$jsonSchema`-validated — a malformed device can't even be
  written."*
- This is the scene that answers "why MongoDB" — time series + tiering +
  change streams + schema validation in one continuous shot.

### Scene 4 — Kill a freezer: detection (2:20–4:00) ⭐ core scene

- **Do:** terminal: `POST /api/demo/inject {"deviceId":"FZ-006","fault":"power_outage"}`
  — or use the Demo drawer in the console (same thing, more visual).
- **While it warms (~90 s on camera):** narrate the wait — *"the classifier
  is watching a 6-minute sliding window; 13 features; it needs ~3 minutes of
  fault physics before it will commit."* Optionally show the fleet grid
  going AT_RISK on FZ-6.
- **CUT** (see §5) → return when the incident is OPEN. **Show:** Incidents
  page: the new card.
  - `faultClass: power_outage`, `confidence: 1.00`
  - `decision: PROPOSED` — *"first incident of this class — awaiting human
    approval"* ← **say this out loud: the AI does NOT act without permission**
  - **topFactors bars**: `power_mean`, `compressor_duty` — *"these drove the
    call; the model shows its work"*
  - FZ-006 tile now **AT_RISK/ALARM** on the fleet grid.
- **Say:** *"~3 minutes after the fault, the classifier calls it a power
  outage at full confidence — but it's the first of its kind, so ColdTrace
  proposes, it doesn't act. Human in the loop by design."*

### Scene 5 — Explain: LLM root-cause + memory (4:00–5:00) ⭐ AI scene

Two panels on the incident page, in this order:

1. **Explain → RCA**: the LLM (OneNexus/OpenAI-compatible engine) writes a
   root-cause analysis **grounded in the actual window stats** — summary,
   root cause, evidence bullets, recommended actions. Say: *"An LLM that can
   only see this device's real telemetry — it cannot invent a number."*
   (If the LLM is down: the deterministic template engine answers instead —
   same shape, data-grounded. The demo never breaks.)
2. **Similar past incidents**: 3 matches, cosine over 13-dim feature vectors,
   *"99.6% — same failure signature as FZ-012 last week"*. Say: *"The agent
   has memory. It recognized this pattern."*

3. **Trace link**: click *"Open trace in Datadog"* — jump to Scene 8 or open
   in a new tab for later.

### Scene 6 — Approve → AUTO-remediate → AUTO-RESOLVE (5:00–6:40) ⭐⭐ the climax

This is the money scene — the full closed loop, and it must be shown as ONE
continuous chain:

- **Do:** AI Control page → approve `power_outage`… or better for pacing:
  approve `compressor_failure` (already approved) and instead
  `POST /api/demo/inject {"deviceId":"FZ-002","fault":"compressor_failure"}`.
- **Wait ~2.5–4 min** → **CUT** → return at `decision: AUTO`.
- **Show & say, in order:**
  1. Card: `decision: AUTO`, confidence ~0.97, ETA ~35 min — *"approved class,
     confidence and ETA gates passed within guardrails"*
  2. **Commands/Events:** `compressor_restart` **QUEUED → SENT → ACKED**,
     latency **~12 ms** — *"the agent remediates over the same MQTT path a
     human would, with a full audit trail"*
  3. FZ-002 tile recovers on the fleet grid (temperature pulls down)
  4. **THE AUTO-RESOLVE** (the part everyone misses): wait ~2–3 min more →
     **CUT** → the incident card flips to **RESOLVED,
     `resolvedBy: system:recovery`** — *"no human clicked anything. After
     60 seconds of provably healthy telemetry the agent closes its own
     incident. Detect → act → RECOVER → learn."*
- Kicker (do not skip — it's the learn half of the loop):
  `POST /api/incidents/{id}/feedback {"resolvedClass":"compressor_failure"}`
  → `"learned": true`, agent logs `ai_learned trainSamples: 801` — *"and it
  just learned from this episode."*

### Scene 7 — Guardrail: the fleet circuit breaker (6:40–7:50) ⭐ trust scene

- **Do:** inject `compressor_failure` on **two devices at once**
  (`FZ-004` + `FZ-005`).
- **Wait ~4 min** → **CUT** → both incidents OPEN:
  - FZ-004: `AUTO` — remediated
  - FZ-005: **`HOLD` — "fleet circuit breaker: too many devices in ALARM —
    auto-actions suspended, human review required"**
- **Say:** *"Two dead compressors at once is probably a grid or a bad batch —
  not twelve independent AI actions. The breaker suspends autonomy and asks
  for a human. And it's LATCHED: when FZ-004 recovers, FZ-005 does NOT
  quietly flip to AUTO — the hold stands until the episode is resolved."*
  (This latched-HOLD is pinned by a regression test — it bit for real in
  rehearsal.)
- **The on-call beat (~20 s, the resolution of the held incident):** with
  FZ-005 held, the human finishes it — **ACK** the incident → **dispatch
  technician** (console Remediate; `technician_dispatched` event,
  `issuedBy: sre-console`) → the device is fixed on site, and after 60 s
  provably healthy the agent **auto-resolves** it (`system:recovery`) —
  the operator's dispatch is in the trail. **Say:** *"The breaker didn't
  just stop the AI — it handed the job to a human. One ack, one dispatch,
  same audit trail. That's the whole resolution story: three paths — AUTO,
  on-call, escalate — one door."*
- Mention the other guardrails while on the AI Control page:
  `minConfidence 0.85` (the same gate that decides whether an incident OPENS
  at all), `maxEtaAlarmMinutes 45`, per-device 30-min cooldown, kill switch.

### Scene 8 — Datadog: dashboard → trace chain (7:50–8:45)

> **Dashboard status (re-applied 2026-11-08 with a working PAT):** the
> *"ColdTrace — Cold-chain Fleet Health"* dashboard + 3 monitors are **LIVE**
> in the org — `./datadog/apply-datadog.sh` pushed them once `.env` carried a
> valid `DD_APP_KEY` (personal access token). Re-run that script after any
> `datadog/dashboard.json` change; metrics/logs/traces have been verified
> flowing with 0 export errors.

- **Show (opener):** the dashboard —
  `app.ap1.datadoghq.com/dashboard/v9y-45k-jsz` — 10 widgets: temperature by
  device with the −10/−14 °C threshold markers, AT_RISK/ALARM counters from
  the Agent's MongoDB custom query, ingest latency + batch sizes, AI
  decisions by classification, compressor-fail log stream. Say: *"the same
  story a NOC would pin on the wall — every widget is the pipeline you just
  watched, in production telemetry."*
- **Show:** from the incident card → *"Open trace in Datadog"* → APM trace:
  `mqtt.ingest → mongo.write → ai.decision`, one span per decision, with
  `ai.top_factor.*` attributes.
- **Show:** the same `traceId` in Log Explorer → logs correlated to the
  trace. Say: *"one identifier follows the reading from the broker to the
  database to the AI decision — this is the audit chain a food-safety
  inspector would demand."*

### Scene 9 — Compliance close + model health (8:45–9:30)

- **Show:** Compliance page → export the HACCP CSV:
  `FZ-009, 19.1 min above −14 °C, worst −11.5, 2026-11-08 06:34 (local ICT)`
  — *"minutes above threshold computed from actual sample deltas, the last
  excursion in the auditor's timezone — this is the document that goes in
  the HACCP binder."*
- **20 s:** Model Health card (100% accuracy, 1,100 fresh windows, clean
  confusion matrix) — *"scored on windows rendered from the same simulator
  physics the fleet runs on — measured, not stored."*
- **10 s:** Events page — the full audit trail you just created on camera.

### Scene 10 — Resolution paths recap + the AI that knows when to stop (9:15–9:45) ⭐ trust

Fast montage (30 s), three cards on screen — the three actions, one line each:

| Path | Line |
|---|---|
| **A — AUTO** | *"Restarted itself in 38 ms — because you approved that class and the gates passed."* |
| **B — ON-CALL** | *"Couldn't fix it? Paged a human with the fix already written: one ack, one dispatch."* |
| **C — ESCALATE** | *"Two at once? It stopped, and asked a person to look at the fleet."* |

Then the A′ beat, from the incident card (or Q&A ammo if you cut it):
`escalationNote` visible — *"restart FAILED 2× — thermal overload latched —
manual action: reboot"* → **Say:** *"The AI tried twice, then did what a good
SRE does: stopped, paged a human, and wrote down everything they'd need.
Escalation isn't the system failing — it's the system working."*

### Scene 11 — Outro (9:45–10:00)

- **Say:** *"Detect in minutes, explain itself, act inside guardrails, prove
  recovery, and know when to hand over. That's ColdTrace."* + repo/QR if wanted.

---


## 4. What the AI actually does (the capability map — use as B-roll/narration)

| # | Capability | Where it shows in the video |
|---|---|---|
| 1 | **Classify** 5 fault classes (softmax over 13 features, warm-started on simulator physics) | Scene 3 — conf 1.00 |
| 2 | **Gate on confidence** — incident only OPENS above the operator's `minConfidence` (0.85), same gate as autonomy | Scene 3/6 — "no noise, no 0.58-confidence ghosts" |
| 3 | **Forecast ETA** to the −10 °C spoilage line; HOLD if it's beyond the horizon (45 min) | Scene 5 — "ETA 35 min, inside guardrail" |
| 4 | **Explain**: topFactors (weight × z per feature) rendered as bars | Scene 3 |
| 5 | **Root-cause** with an LLM grounded in real window stats (template fallback if the API is down) | Scene 4 |
| 6 | **Remember**: cosine similarity over past incident vectors → "similar incidents" | Scene 4 |
| 7 | **Decide within guardrails**: class approval, confidence, ETA, cooldown, fleet circuit breaker, kill switch | Scenes 5–6 |
| 8 | **Act**: MQTT `compressor_restart` with a full command lifecycle (QUEUED→SENT→ACKED, 0–38 ms) | Scene 5 |
| 9 | **Prove recovery**: 60 s healthy streak → auto-resolve `system:recovery` | Scene 5 ⭐ |
| 10 | **Learn**: operator feedback → `Learn()` retrains, `trainSamples: 801` | Scene 5 kicker |
| 11 | **Full-chain tracing**: one `traceId` broker → DB → decision → Datadog | Scene 7 |
| 12 | **Handle its own failure** ⭐: a refused restart (device acks `ok:false`) → retry after backoff → escalate to on-call with a pre-drafted page — never loops, never sits quiet | Scene 7 B-roll / Q&A |

### The fault signatures — why "temperature change speed" IS the classifier

Each fault class has a characteristic ramp (`internal/sim/device.go`), and
the model's dominant features are all speed features (`trend_lag_cpm`,
`net_trend_cpm`, `slope_cpm` — the top weights on any real decision):

| Fault class | Temp-speed signature | What finishes the discrimination | Remote fix? |
|---|---|---|---|
| `compressor_failure` | **+0.3 °C/min** ramp | compressor stops, power draw present | ✅ `compressor_restart` |
| `door_left_open` | **+0.1 °C/min** ramp | `doorOpen: true`, compressor pulling hard | ❌ staff close it |
| `sensor_drift` | **−0.25 °C/min** *reported* (sensor under-reads) | compressor normal, neighbours cold | ❌ replace probe |
| `power_outage` | warm ramp ≈ compressor's | **power draw zero** — everything dead | ❌ restore power on site |
| healthy | ±~1.4 °C thermostat sawtooth | no sustained ramp, full window | — |

**Narration line:** *"Every failure has a fingerprint in how fast — and how
honestly — the temperature moves. Speed alone splits three classes; the
other two need the cross-signals, and `etaAlarmMin` is just
`(alarm − temp) / ramp` — the model doing division so it can tell you
there's time to act."*

### The resolution playbook — the three action paths you demo

The video's through-line is RESOLUTION: something breaks → one of three
actions fixes it. Every path leaves the same audit trail (commands +
events), which is the point: **AI and humans act through one door.**

| Path | When | Decision | The action (demo this) | Audit evidence on camera |
|---|---|---|---|---|
| **A — AUTO** *(restart)* | approved class + conf ≥ 0.85 + ETA ≤ 45 + breaker clear | `AUTO` | AI issues `compressor_restart` itself → device ACKED ~12 ms → temp pulls down → 60 s healthy streak | `QUEUED→SENT→ACKED`, `remediatedBy: ai-agent`, `resolvedBy: system:recovery` |
| **B — ON-CALL** *(human dispatch)* | unapproved class, or first-of-kind, or ETA > 45 | `HOLD` / `PROPOSED` | Operator **ACKs** → **dispatches technician** (console Remediate → `technician_dispatched`) → device fixed on site → recovery | `acknowledged: true`, event `technician_dispatched`, command `issuedBy: sre-console`, resolve + `feedback` |
| **C — ESCALATE** *(breaker)* | ≥2 devices faulting at once | `HOLD` (latched per episode) | Autonomy suspended → human reviews the fleet → acts per device | `circuitBreaker: true`, "repeat actions blocked by cooldown" |

Exact calls for path B (the one that needs a human):

```bash
INCIDENT_ID=<from the incident card>
curl -X POST localhost:8092/api/incidents/$INCIDENT_ID/ack -H 'X-Operator: on-call'  # take it (no body)
curl -X POST localhost:8099/devices/FZ-005/remediate                               # dispatch technician (no body)
# …device fixed on site, temp recovers. After 60 s provably healthy the agent
#   AUTO-RESOLVES it (resolvedBy system:recovery) — or close it yourself, no body:
curl -X POST localhost:8092/api/incidents/$INCIDENT_ID/resolve -H 'X-Operator: on-call'
curl -X POST localhost:8092/api/incidents/$INCIDENT_ID/feedback \
     -H 'Content-Type: application/json' -d '{"resolvedClass":"door_left_open"}'   # → learned: true
```

**Say:** *"When the AI can act, it acts — and shows the receipt. When it
can't, a human is on-call: one ack, one dispatch, the same audit trail.
Three paths, one door, nothing happens off the record."*

### Path A′ — when AUTO itself fails (the agentic-failure scene)

The restart is a command, and commands can be **refused by the device** —
a latched thermal overload (`compressor_failure_hard`) acks `ok:false` and
the fault STANDS. The agent does not loop and does not sit on
"awaiting recovery":

1. restart #1 → **REFUSED** (command `FAILED`, device ack in `ackDetail`)
2. 30 s backoff → **retry #2** → REFUSED again
3. → **ESCALATED to on-call**: incident gets `escalated: true` +
   `escalationNote` (class, confidence, ETA, the refusal, the manual
   action, the closest past episode) and the audit log gets a
   **critical** `incident_escalated` page. A human power-cycles the
   controller (`reboot`) — the exact action the note prescribes.

```bash
curl -X POST localhost:8092/api/demo/inject -H 'Content-Type: application/json' \
     -d '{"deviceId":"FZ-005","fault":"compressor_failure_hard"}'      # the failure that fights back
# …detect → AUTO → restart REFUSED → retry → REFUSED → ESCALATED (~1 min after detect)
curl -X POST localhost:8092/api/devices/FZ-005/commands -H 'Content-Type: application/json' \
     -H 'X-Operator: on-call' -d '{"action":"reboot"}'                 # the human fix the page prescribed
```

**Say:** *"The AI tried twice, then did what a good SRE does: stopped,
paged a human, and wrote down everything they'd need — the class, the
confidence, how long until damage, what the device said, and the fix.
Escalation isn't failure of the system; it's the system working."*

---

## 5. Editing: hiding the waits

Detection is real physics — **~2.5–4 min** from inject to incident, and the
auto-resolve needs another ~2–3 min. Never make the viewer watch that.

| Trick | How |
|---|---|
| **Hard cut + clock overlay** | Cut inject → incident. Overlay "T+3:10" text (OBS text or CapCut sticker) so the cut is honest |
| **Speed ramp** | 8–12× time-lapse of the fleet grid while the tile warms — shows the physics AND keeps pace |
| **Narration over the cut** | Explain the status ladder / guardrails while the time-lapse plays |
| **Scene 6 is the only long one** | The AUTO→ACKED→recover→RESOLVED chain needs ~5 min real time; record it in one take with a 10× ramp, then trim |

---

## 6. Fallbacks (so the recording cannot die)

| If this fails | Do this |
|---|---|
| LLM RCA not ready | Template engine answers with the same shape — narrate it as the designed fallback ("keyless mode") |
| Detection > 5 min | Windows weren't warm: `POST /api/demo/reset` → wait 7 full minutes → re-inject. Never re-inject without resetting |
| Wrong class classified | Keep it! Show `ai_class_flip` + the anchored-episode dedup ("one episode per device, the anchor stands") — it's a feature |
| Incident won't open (conf < 0.85) | That's the gate working; inject on a different device or narrate the confidence gate |
| Datadog slow | The trace is stored on the incident doc (`traceId`) — open it after recording, splice the clip |
| Stack broken | `scripts/demo_preflight.sh` prints the exact fix for each check — run it on camera if you must (it's honest ops) |

---

## 7. Shooting order — record sequence ≠ scene order (one 40-min session)

Detection waits and window warm-ups make naive scene-order recording waste
hours. Record in this sequence instead, then cut into scene order:

| Step | Record (wall clock) | Becomes scene |
|---|---|---|
| 1 (T+0) | `demo_cleanup.sh` → `demo_preflight.sh` on camera (12/12 green) — then **start the 7-min warm-up timer and record Scene 1 + Scene 2 narration** | 1, 2 |
| 2 (T+7) | Inject `power_outage` FZ-006 → **while it warms, record Scene 3** (History tier flip + Mongo story — needs no fault) **and the AI-Control guardrails walkthrough** | 3, 7 (guardrails half) |
| 3 (T+11) | FZ-006 opens → record the incident card, topFactors, **Explain → RCA**, similar-incidents, PROPOSED reason | 4, 5 |
| 4 (T+15) | Inject `compressor_failure` FZ-002 (class already approved) → record the fleet grid AT_RISK/ALARM transition narration | 2 (transition), 6 setup |
| 5 (T+19) | AUTO card appears → record decision/gates → **Commands: QUEUED→SENT→ACKED ~12 ms** → tile recovers | 6 |
| 6 (T+24) | **Keep rolling through the auto-resolve** (2–3 min — speed-ramp it later) → `system:recovery` flips the card → feedback → `learned: true` | 6 (the close of the loop) |
| 7 (T+27) | Inject FZ-004 **and** FZ-005 → record the Events/Compliance walkthrough while they warm | 9 |
| 8 (T+31) | Both open → record FZ-004 AUTO vs FZ-005 **HOLD latched** (+ one beat showing FZ-005 still HOLD after FZ-004 resolves) → then the **on-call close**: ACK FZ-005 → dispatch technician (console Remediate) → recovery → `system:recovery` | 7 |
| 9 (T+35) | From an incident card → **Open trace in Datadog** → span chain + log correlation by trace_id | 8 |
| 10 (T+38) | Compliance page → CSV export (seeded FZ-009 row) → Model Health (100%, 1,100 windows) → outro narration | 9, 10 |
| 11 (T+40) | `demo_cleanup.sh` + `demo_preflight.sh` again if you need pickup takes — **but re-wait 7 min before any new inject** | pickups |

Why this order works: every inject happens with warm windows; every wait is
filled with a scene that needs no live fault; the auto-resolve (longest real
wait) records last so a failure there doesn't sink earlier scenes.

---

## 8. Record-of-truth: numbers to quote (measured this rehearsal)

- Detection: **148–255 s** warm windows, **387 s** from a cold window
  (FZ-002/011 today: 243–255 s post-fix)
- Confidence at detection: **0.90–1.00** (FZ-002 0.908 / ETA 44.8 min —
  gate-relevant; FZ-011 0.995 / ETA 34.8)
- Command ACK latency: **0–12 ms** (re-verified after the ops-api
  reconnect fix — a forced broker restart no longer rots commands to
  TIMEOUT; `set_fault` ACKED 0 ms across the reset)
- Auto-resolve: **~91–373 s** after remediation (`system:recovery`)
- Agentic failure handling: hard restart **refused → retry after 30 s
  backoff → refused → escalated** in under ~90 s from detection, with the
  pre-drafted `escalationNote` + critical `incident_escalated` event —
  then the on-call `reboot` (exactly what the note prescribed) closes it
- Circuit breaker: FZ-004 AUTO + FZ-005 **HOLD latched** — verified it does
  NOT flip when FZ-004 resolves
- On-call path (path B): `incident_ack` → `technician_dispatched` → device
  fix → `system:recovery` — the full human-resolution audit chain, verified
  end-to-end
- Model eval: **100%**, 1,100 fresh simulator windows, 5 classes
- Compliance CSV: 19.1 min excursion, worst −11.5 °C, local-time column
- Preflight: **12/12 checks green** before every recording session
