# ColdTrace — Demo Video Plan

A complete production plan for a **6–8 minute product demo video**: what to
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
> itself (**command → device ACK in ~12 ms**), then **auto-resolves** the
> incident when the cabinet is provably healthy again — and the whole chain
> is one trace in Datadog.

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

Total runtime target: **6:30–8:00**. Real detection waits are hidden with
**cuts** (see §5), so wall-clock recording is ~35 min for a ~7 min video.

### Scene 1 — The problem (0:00–0:25) *no live stack needed*

- **Show:** `docs/screenshots` or the deck's opening: freezer warms −18 →
  −14 → −10 while nobody watches.
- **Say:** the pitch from §0. End on: *"This is ColdTrace. Let me kill a
  compressor and watch it handle it."*
- Why first: judges decide in the first 30 s whether the rest is worth
  watching. A dead freezer is the whole business case.

### Scene 2 — Fleet Overview (0:25–1:00)

- **Show:** console `/` — 12 green tiles, KPI strip, Sites rollup, the live
  event feed ticking (SSE, not polling).
- **Say:** *"Twelve freezers, three sites, all green. Status is COMPUTED
  never stored — STALE → ALARM → AT_RISK → OK, with a deadband so a resting
  compressor doesn't paint the fleet yellow. It's live over MongoDB change
  streams — no refresh, no polling."*
- **Proof moment:** point at a tile's temperature updating every 5 s.

### Scene 3 — Kill a freezer: detection (1:00–2:30) ⭐ core scene

- **Do:** terminal: `POST /api/demo/inject {"deviceId":"FZ-006","fault":"power_outage"}`
  — or use the Demo drawer in the console (same thing, more visual).
- **Wait ~3–4 min** → **CUT** (see §5) → return when the incident is OPEN.
- **Show:** Incidents page: the new card.
  - `faultClass: power_outage`, `confidence: 1.00`
  - `decision: PROPOSED` — *"first incident of this class — awaiting human
    approval"* ← **say this out loud: the AI does NOT act without permission**
  - **topFactors bars**: `power_mean`, `compressor_duty` — *"these drove the
    call; the model shows its work"*
  - FZ-006 tile now **AT_RISK/ALARM** on the fleet grid.
- **Say:** *"~3 minutes after the fault, the classifier calls it a power
  outage at full confidence — but it's the first of its kind, so ColdTrace
  proposes, it doesn't act. Human in the loop by design."*

### Scene 4 — Explain: LLM root-cause + memory (2:30–3:30) ⭐ AI scene

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

3. **Trace link**: click *"Open trace in Datadog"* — jump to Scene 6 or open
   in a new tab for later.

### Scene 5 — Approve → AUTO-remediate → AUTO-RESOLVE (3:30–5:00) ⭐⭐ the climax

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
- Optional kicker: `POST /api/incidents/{id}/feedback {"resolvedClass":"compressor_failure"}`
  → `"learned": true`, agent logs `ai_learned trainSamples: 801` — *"and it
  just learned from this episode."*

### Scene 6 — Guardrail: the fleet circuit breaker (5:00–6:00) ⭐ trust scene

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
- Mention the other guardrails while on the AI Control page:
  `minConfidence 0.85` (the same gate that decides whether an incident OPENS
  at all), `maxEtaAlarmMinutes 45`, per-device 30-min cooldown, kill switch.

### Scene 7 — Datadog: the trace chain (6:00–6:45)

> **Dashboard decision (reviewed 2026-11-08):** the custom *"ColdTrace —
> Cold-chain Fleet Health"* dashboard + 3 monitors are **CUT from the video**
> — `datadog/apply-datadog.sh` requires `DD_APP_KEY` and `.env` has none, so
> they were never applied and a recording would show a 404. To enable later:
> add `DD_APP_KEY` to `.env`, run `./datadog/apply-datadog.sh`, then re-approve
> this scene. What IS flowing (verified: 0 export errors) and what the video
> uses instead:

- **Show:** from the incident card → *"Open trace in Datadog"* → APM trace:
  `mqtt.ingest → mongo.write → ai.decision`, one span per decision, with
  `ai.top_factor.*` attributes.
- **Show:** the same `traceId` in Log Explorer → logs correlated to the
  trace. Say: *"one identifier follows the reading from the broker to the
  database to the AI decision — this is the audit chain a food-safety
  inspector would demand."*

### Scene 8 — Compliance close (6:45–7:15)

- **Show:** Compliance page → export the HACCP CSV:
  `FZ-009, 19.1 min above −14 °C, worst −11.5, 2026-11-08 06:34 (local ICT)`
  — *"minutes above threshold computed from actual sample deltas, the last
  excursion in the auditor's timezone — this is the document that goes in
  the HACCP binder."*
- Optional 15 s: Model Health card (100% accuracy, 1,100 fresh windows, clean
  confusion matrix) + Events page (the full audit trail you just created).

### Scene 9 — Outro (7:15–7:45)

- **Say:** *"Detect in minutes, explain itself, act inside guardrails, prove
  recovery, close the audit trail. That's ColdTrace."* + repo/QR if wanted.

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
| 8 | **Act**: MQTT `compressor_restart` with a full command lifecycle (QUEUED→SENT→ACKED, ~12 ms) | Scene 5 |
| 9 | **Prove recovery**: 60 s healthy streak → auto-resolve `system:recovery` | Scene 5 ⭐ |
| 10 | **Learn**: operator feedback → `Learn()` retrains, `trainSamples: 801` | Scene 5 kicker |
| 11 | **Full-chain tracing**: one `traceId` broker → DB → decision → Datadog | Scene 7 |

---

## 5. Editing: hiding the waits

Detection is real physics — **~2.5–4 min** from inject to incident, and the
auto-resolve needs another ~2–3 min. Never make the viewer watch that.

| Trick | How |
|---|---|
| **Hard cut + clock overlay** | Cut inject → incident. Overlay "T+3:10" text (OBS text or CapCut sticker) so the cut is honest |
| **Speed ramp** | 8–12× time-lapse of the fleet grid while the tile warms — shows the physics AND keeps pace |
| **Narration over the cut** | Explain the status ladder / guardrails while the time-lapse plays |
| **Scene 5 is the only long one** | The AUTO→ACKED→recover→RESOLVED chain needs ~5 min real time; record it in one take with a 10× ramp, then trim |

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

## 7. Record-of-truth: numbers to quote (measured this rehearsal)

- Detection: **148–229 s** warm windows, **387 s** from a cold window
- Confidence at detection: **0.93–1.00**
- Command ACK latency: **0–12 ms**
- Auto-resolve: **~91–182 s** after remediation (`system:recovery`)
- Circuit breaker: FZ-004 AUTO + FZ-005 **HOLD latched** — verified it does
  NOT flip when FZ-004 resolves
- Model eval: **100%**, 1,100 fresh simulator windows, 5 classes
- Compliance CSV: 19.1 min excursion, worst −11.5 °C, local-time column
- Preflight: **12/12 checks green** before every recording session
