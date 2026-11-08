// ColdTrace AI Ops Agent — event-driven (MongoDB Change Streams) service:
// classifies device faults online, predicts time-to-alarm, scores anomalies,
// remembers past incidents (in-process cosine over feature vectors), and
// remediates autonomously under a hybrid approval policy.
//
// Service name in traces: coldtrace-ai-agent (separate node in the service map
// = Agent Observability story).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/onemount/coldtrace/internal/ai"
	"github.com/onemount/coldtrace/internal/otlp"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// incidentDoc mirrors the `ai_incidents` collection.
type incidentDoc struct {
	ID          bson.ObjectID     `bson:"_id,omitempty"     json:"id,omitempty"`
	DeviceID    string            `bson:"deviceId"          json:"deviceId"`
	Class       string            `bson:"faultClass"        json:"faultClass"`
	Confidence  float64           `bson:"confidence"        json:"confidence"`
	EtaAlarmMin *float64          `bson:"etaAlarmMin,omitempty" json:"etaAlarmMin,omitempty"`
	Features    []float64         `bson:"features"          json:"features"`
	Decision    string            `bson:"decision"          json:"decision"` // PROPOSED|AUTO|HOLD
	Reason      string            `bson:"reason"            json:"reason"`
	Similar     []ai.SimilarMatch `bson:"similar,omitempty" json:"similar,omitempty"`
	// CircuitBreaker pins an episode that tripped the fleet breaker. Without
	// it the decision flipped back to AUTO as soon as another device
	// recovered, auto-remediating the very incident the breaker had held.
	CircuitBreaker bool           `bson:"circuitBreaker,omitempty" json:"circuitBreaker,omitempty"`
	Status         string         `bson:"status"            json:"status"` // OPEN|RESOLVED
	RemediatedAt   *time.Time     `bson:"remediatedAt,omitempty" json:"remediatedAt,omitempty"`
	RemediatedBy   string         `bson:"remediatedBy,omitempty" json:"remediatedBy,omitempty"`
	OpenedAt       time.Time      `bson:"openedAt"          json:"openedAt"`
	TraceID        string         `bson:"traceId,omitempty" json:"traceId,omitempty"`
	TopFactors     []ai.TopFactor `bson:"topFactors,omitempty" json:"topFactors,omitempty"`
	RCA            *IncidentRCA   `bson:"rca,omitempty"     json:"rca,omitempty"`
	Acknowledged   *bool          `bson:"acknowledged,omitempty" json:"acknowledged,omitempty"`
}

// IncidentRCA is the LLM root-cause analysis stored with the incident.
type IncidentRCA struct {
	Summary   string    `bson:"summary"   json:"summary"`
	RootCause string    `bson:"rootCause" json:"rootCause"`
	Evidence  []string  `bson:"evidence"  json:"evidence"`
	Actions   []string  `bson:"actions"   json:"actions"`
	Model     string    `bson:"model"     json:"model"`
	At        time.Time `bson:"at"       json:"at"`
}

type agent struct {
	log           *slog.Logger
	db            *mongo.Database
	coll          *mongo.Collection // telemetry
	inc           *mongo.Collection // ai_incidents
	approv        *mongo.Collection // ai_approvals
	cmds          *mongo.Collection // commands (auto-remediation gets a real lifecycle doc)
	mqtt          *autopaho.ConnectionManager
	model         *ai.Classifier
	mu            sync.Mutex // guards wins, scorer, autoco (fed by poll, read by loop)
	wins          map[string]*ai.Window
	scorer        map[string]*ai.AnomalyScorer
	autoco        map[string]time.Time // device -> last auto-remediation
	healthySince  map[string]time.Time // device -> start of current healthy streak
	policyCache   *ai.PolicyConfig     // cached guardrails (30s) — read from every evaluate
	policyCacheAt time.Time
	approvCache   map[string]bool // cached approvals (30s)
	approvCacheAt time.Time
	tracer        trace.Tracer
	llm           *ai.LLM
	mem           *ai.IncidentMemory
}

func main() {
	mqttURL := flag.String("broker", env("MQTT_BROKER", "mqtt://broker:1883"), "mqtt broker url")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "coldtrace-ai-agent")
	ctx := context.Background()

	stopTrace, err := otlp.StartTrace(ctx, "coldtrace-ai-agent")
	if err != nil {
		logger.Warn("otlp trace init", "err", err)
	}
	stopMetric, err := otlp.StartMetric(ctx, "coldtrace-ai-agent")
	if err != nil {
		logger.Warn("otlp metric init", "err", err)
	}
	_, logProvider, err := otlp.StartLogs(ctx, "coldtrace-ai-agent")
	if err != nil {
		logger.Warn("otlp logs init", "err", err)
	}
	logger = otlp.NewTraceLogger(logger, logProvider)
	defer func() {
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if logProvider != nil {
			_ = logProvider.Shutdown(shCtx)
		}
		if stopMetric != nil {
			_ = stopMetric(shCtx)
		}
		if stopTrace != nil {
			_ = stopTrace(shCtx)
		}
	}()

	mclient, err := mongo.Connect(options.Client().ApplyURI(env("MONGO_URI", "mongodb://mongodb:27017")))
	if err != nil {
		logger.Error("mongo connect", "err", err)
		os.Exit(1)
	}
	db := mclient.Database(env("MONGO_DB", "coldtrace"))

	u, err := url.Parse(*mqttURL)
	if err != nil {
		logger.Error("bad broker url", "err", err)
		os.Exit(1)
	}
	cfg := autopaho.ClientConfig{
		BrokerUrls:        []*url.URL{u},
		KeepAlive:         30,
		ConnectRetryDelay: 2 * time.Second,
		ClientConfig: paho.ClientConfig{
			ClientID:      "coldtrace-ai-agent",
			OnClientError: func(err error) { logger.Warn("mqtt client error", "err", err) },
		},
	}
	cm, err := autopaho.NewConnection(ctx, cfg)
	if err != nil {
		logger.Error("mqtt connection", "err", err)
		os.Exit(1)
	}
	if err := cm.AwaitConnection(ctx); err != nil {
		logger.Error("mqtt await", "err", err)
		os.Exit(1)
	}

	a := &agent{
		log: logger, db: db, coll: db.Collection("telemetry"),
		inc: db.Collection("ai_incidents"), approv: db.Collection("ai_approvals"),
		cmds: db.Collection("commands"),
		mqtt: cm, model: ai.NewClassifier(logger),
		scorer: map[string]*ai.AnomalyScorer{}, wins: map[string]*ai.Window{},
		autoco:       map[string]time.Time{},
		healthySince: map[string]time.Time{},
		tracer:       otel.Tracer("coldtrace.ai"),
		llm: ai.NewLLM(ai.LLMConfig{
			// OpenAI-compatible engine (default api.openai.com; override, e.g.
			// https://catalog.onenexus.cloud/v1 for the OneNexus catalog).
			BaseURL: env("OPENAI_BASE_URL", ""),
			APIKey:  env("OPENAI_API_KEY", ""),
			Model:   env("OPENAI_MODEL", "gpt-4o-mini"),
			// Anthropic Messages API — wins when ANTHROPIC_API_KEY is set.
			AnthropicBaseURL: env("ANTHROPIC_BASE_URL", ""),
			AnthropicAPIKey:  env("ANTHROPIC_API_KEY", ""),
			AnthropicModel:   env("ANTHROPIC_MODEL", "claude-sonnet-4-6"),
			Timeout:          60 * time.Second,
		}),
		mem: ai.NewIncidentMemory(db.Collection("ai_incidents")),
	}
	logger.Info("incident_memory_ready",
		"detail", "cosine over "+strconv.Itoa(ai.NumFeatures)+"-dim feature vectors, computed in-process; no search index required")
	if a.llm.Enabled() {
		logger.Info("llm configured", "provider", a.llm.Provider(), "baseURL", a.llm.BaseURL(), "model", a.llm.Model())
	} else {
		logger.Info("llm not configured; RCA disabled (set ANTHROPIC_API_KEY or OPENAI_API_KEY)")
	}

	// indexes for incident memory + incident listing
	_, _ = a.inc.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "openedAt", Value: -1}},
	})

	// background evaluation loop (30s cadence)
	go a.loop(ctx)
	// change-stream watcher keeps windows warm + proves event-driven Mongo use
	go a.watch(ctx)

	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.Recoverer)
	r.Get("/ai/incidents", a.listIncidents)
	r.Get("/ai/incidents/{incidentID}/rca", a.getRCA)
	r.Post("/ai/approvals/{faultClass}", a.approve)
	r.Delete("/ai/approvals/{faultClass}", a.revoke)
	r.Get("/ai/approvals", a.listApprovals)
	r.Post("/ai/feedback/{incidentID}", a.feedback) // resolution label -> Learn()
	r.Post("/ai/reset", a.reset)                    // demo cleanup: forget in-memory state

	logger.Info("ai agent online", "broker", *mqttURL)
	srv := &http.Server{Addr: ":" + env("AI_PORT", "8091"), Handler: otelhttp.NewHandler(r, "coldtrace-ai-agent")}
	// Graceful shutdown so the deferred OTLP providers flush before exit.
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		logger.Error("http server", "err", err)
		os.Exit(1)
	case s := <-sig:
		logger.Info("shutdown", "signal", s.String())
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("shutdown", "err", err)
		}
		cancel()
	}
}

// watch tails ai_incidents inserts (event-driven reaction to decisions made
// elsewhere) and keeps a polling cursor on telemetry to feed windows.
// Note: `telemetry` is a Time Series *view*; MongoDB change streams cannot
// watch views (CommandNotSupportedOnView), so windows are filled by cursor
// polling while ai_incidents (plain collection) keeps the live stream demo.
// Reconnects with exponential backoff + resume token (no busy-spin, no
// backlog re-decode after a flap — the token skips already-seen events).
func (a *agent) watch(ctx context.Context) {
	go a.pollTelemetry(ctx)

	backoff := 1 * time.Second
	var resumeToken bson.M
	for {
		opts := options.ChangeStream().
			SetFullDocument(options.UpdateLookup)
		if resumeToken != nil {
			opts.SetResumeAfter(resumeToken)
		}
		cs, err := a.inc.Watch(ctx, mongo.Pipeline{}, opts)
		if err != nil {
			a.log.Warn("incident change stream unavailable; retrying", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 15*time.Second)
			continue
		}
		backoff = 1 * time.Second
		for cs.Next(ctx) {
			if t := cs.ResumeToken(); t != nil {
				var doc bson.M
				if err := bson.Unmarshal(t, &doc); err == nil {
					resumeToken = doc
				} else {
					resumeToken = nil
				}
			}
			var ev struct {
				OperationType string `bson:"operationType"`
			}
			if err := cs.Decode(&ev); err != nil {
				a.log.Warn("incident change decode failed", "err", err)
				break // reconnect with the saved token
			}
			a.log.Info("incident_change_event", "op", ev.OperationType)
		}
		if ctx.Err() != nil {
			_ = cs.Close(ctx)
			return
		}
		a.log.Warn("incident change stream ended; reconnecting", "err", cs.Err())
		_ = cs.Close(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 15*time.Second)
	}
}

// pollTelemetry fills device windows by tailing the TS collection with a
// tailable-style cursor (batched by _id/ts ordering every cycle).
// Cold start seeds lastTS to now-window and the FIRST query also filters
// ts > lastTS — an unfiltered oldest-500 query would ingest stale history
// instead of the recent window (the exact bug the seed was added to fix).
func (a *agent) pollTelemetry(ctx context.Context) {
	lastTS := time.Now().UTC().Add(-10 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		q := bson.D{{Key: "ts", Value: bson.D{{Key: "$gt", Value: lastTS}}}}
		cur, err := a.coll.Find(ctx, q, options.Find().SetSort(bson.D{{Key: "ts", Value: 1}}).SetLimit(500))
		if err == nil {
			var docs []struct {
				Meta struct {
					DeviceID string `bson:"deviceId"`
				} `bson:"meta"`
				TS                time.Time `bson:"ts"`
				TempC             float64   `bson:"tempC"`
				PowerDraw         float64   `bson:"powerDraw"`
				CompressorRunning bool      `bson:"compressorRunning"`
				DoorOpen          bool      `bson:"doorOpen"`
			}
			if err := cur.All(ctx, &docs); err == nil && len(docs) > 0 {
				for _, d := range docs {
					a.ingest(d.Meta.DeviceID, ai.Sample{TS: d.TS, TempC: d.TempC,
						PowerDraw: d.PowerDraw, CompressorRunning: d.CompressorRunning, DoorOpen: d.DoorOpen})
					if d.TS.After(lastTS) {
						lastTS = d.TS
					}
				}
			}
			cur.Close(ctx)
		}
		time.Sleep(5 * time.Second)
	}
}

// ingest feeds windows + anomaly scorers.
func (a *agent) ingest(deviceID string, s ai.Sample) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.wins[deviceID]
	if w == nil {
		w = ai.NewWindow(deviceID, 72) // 6 min at 5s
		a.wins[deviceID] = w
	}
	w.Push(s)
	if a.scorer[deviceID] == nil {
		a.scorer[deviceID] = ai.NewAnomalyScorer()
	}
	_ = a.scorer[deviceID].Score(s.TempC)
}

// loop evaluates every device window every 30s. Windows and scorers are
// mutated by pollTelemetry→ingest under a.mu; snapshots are taken under the
// same lock so evaluate never races a concurrent Push (races surfaced under
// -race: concurrent map read + slice append during Features()).

// evalWindow is a consistent copy of one device's window state.
type evalWindow struct {
	DeviceID string
	Samples  []ai.Sample
}

func (a *agent) snapshotWindows() []evalWindow {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]evalWindow, 0, len(a.wins))
	for id, w := range a.wins {
		cp := make([]ai.Sample, len(w.Samples))
		copy(cp, w.Samples)
		out = append(out, evalWindow{DeviceID: id, Samples: cp})
	}
	return out
}

func (a *agent) loop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		for _, w := range a.snapshotWindows() {
			if len(w.Samples) == 0 {
				continue
			}
			last := w.Samples[len(w.Samples)-1].TS
			if now.Sub(last) > 2*time.Minute || len(w.Samples) < 24 {
				continue
			}
			a.evaluate(ctx, w.DeviceID, w.Samples, now)
		}
	}
}

// approvedClasses loads the auto-remediation allowlist, cached for 30s —
// the 30s evaluation loop runs it once per device; without the cache a
// 12-device fleet fires 24 extra Mongo queries per tick.
func (a *agent) approvedClasses(ctx context.Context) map[string]bool {
	a.mu.Lock()
	cachedAt, cached := a.approvCacheAt, a.approvCache
	a.mu.Unlock()
	if cached != nil && time.Since(cachedAt) < 30*time.Second {
		return cached
	}
	out := map[string]bool{}
	cur, err := a.approv.Find(ctx, bson.D{})
	if err != nil {
		return out
	}
	var docs []struct {
		Class string `bson:"faultClass"`
	}
	_ = cur.All(ctx, &docs)
	for _, d := range docs {
		out[d.Class] = true
	}
	a.mu.Lock()
	a.approvCache, a.approvCacheAt = out, time.Now().UTC()
	a.mu.Unlock()
	return out
}

// autoResolveIfRecovered marks a device's OPEN incidents RESOLVED once it has
// been confidently healthy for 60s (recoveryArcSecs). Emits an ai_recovery
// event so the dashboard shows the full detect->act->recover->learn arc.
func (a *agent) autoResolveIfRecovered(ctx context.Context, deviceID string, now time.Time) {
	const recoveryArcSecs = 60 * time.Second
	a.mu.Lock()
	t, ok := a.healthySince[deviceID]
	a.mu.Unlock()
	if !ok || now.Sub(t) < recoveryArcSecs {
		return // not yet a confirmed recovery
	}
	res, err := a.inc.UpdateMany(ctx,
		bson.M{"deviceId": deviceID, "status": "OPEN"},
		bson.M{"$set": bson.M{"status": "RESOLVED", "resolvedAt": now, "resolvedBy": "system:recovery"}})
	if err != nil || res.ModifiedCount == 0 {
		return
	}
	a.log.Info("ai_recovery", "deviceId", deviceID, "resolvedIncidents", res.ModifiedCount,
		"reason", "device confidently healthy for 60s — auto-resolved")
	// reset the streak so a future fault starts a fresh recovery window
	a.mu.Lock()
	a.healthySince[deviceID] = now
	a.mu.Unlock()
}

// policyConfig loads the operator-editable guardrails from ai_policy.
// A missing/invalid document falls back to DefaultPolicy. Kill switch ON is
// modelled as FleetCircuitBreaker=0, which Evaluate always trips (HOLD for
// every autonomous action).
func (a *agent) policyConfig(ctx context.Context) ai.PolicyConfig {
	a.mu.Lock()
	cachedAt, cached := a.policyCacheAt, a.policyCache
	a.mu.Unlock()
	if cached != nil && time.Since(cachedAt) < 30*time.Second {
		return *cached
	}
	cfg := ai.DefaultPolicy()
	var doc struct {
		MinConfidence       float64 `bson:"minConfidence"`
		MaxEtaAlarmMinutes  float64 `bson:"maxEtaAlarmMinutes"`
		AutoCooldownMinutes int     `bson:"autoCooldownMinutes"`
		FleetCircuitBreaker int     `bson:"fleetCircuitBreaker"`
		KillSwitch          bool    `bson:"killSwitch"`
	}
	if err := a.db.Collection("ai_policy").FindOne(ctx, bson.D{}).Decode(&doc); err != nil {
		a.mu.Lock()
		a.policyCache, a.policyCacheAt = &cfg, time.Now().UTC()
		a.mu.Unlock()
		return cfg
	}
	if doc.MinConfidence > 0 && doc.MinConfidence <= 1 {
		cfg.MinConfidence = doc.MinConfidence
	}
	if doc.MaxEtaAlarmMinutes > 0 {
		cfg.MaxEtaAlarmMinutes = doc.MaxEtaAlarmMinutes
	}
	if doc.AutoCooldownMinutes > 0 {
		cfg.AutoCooldown = time.Duration(doc.AutoCooldownMinutes) * time.Minute
	}
	if doc.FleetCircuitBreaker > 0 {
		cfg.FleetCircuitBreaker = doc.FleetCircuitBreaker
	}
	if doc.KillSwitch {
		cfg.FleetCircuitBreaker = 0 // 0 => Evaluate always trips the breaker
	}
	a.mu.Lock()
	a.policyCache, a.policyCacheAt = &cfg, time.Now().UTC()
	a.mu.Unlock()
	return cfg
}

// evaluate runs the full AI pipeline for one device's copied samples and
// applies the policy. `mu` guards map fields (autoco/healthySince/scorer);
// samples are an already-copied slice from snapshotWindows.
func (a *agent) evaluate(ctx context.Context, deviceID string, samples []ai.Sample, now time.Time) {
	w := &ai.Window{DeviceID: deviceID, Samples: samples}
	feats := ai.Normalize(ai.Features(w))
	class, probs := a.model.Predict(feats)
	_, eta := ai.Forecast(w, 15, -10)
	// warm the scorer under the same lock ingest() uses (it owns map+scorer)
	a.mu.Lock()
	sc := a.scorer[deviceID]
	a.mu.Unlock()
	if sc != nil {
		_ = sc.Score(samples[len(samples)-1].TempC) // keep baseline warm
	}
	conf := probs[class]

	// recovery check: if the device has been confidently healthy for a
	// sustained stretch, auto-resolve its OPEN incidents (closes the loop:
	// detect -> act -> recover -> learn, no manual clicks needed).
	if class == ai.ClassHealthy && conf >= 0.55 {
		a.mu.Lock()
		t, ok := a.healthySince[deviceID]
		if !ok {
			a.healthySince[deviceID] = now
		}
		a.mu.Unlock()
		if ok && now.Sub(t) >= 60*time.Second {
			a.autoResolveIfRecovered(ctx, deviceID, now)
		}
		return
	}

	// Open an incident only when the policy would trust the call. The old
	// 0.55 floor opened incidents the guardrails then refused, and it once
	// opened a power_outage at 0.58 seconds after a recovery — noise on the
	// stage. Binding the open gate to the same MinConfidence the policy uses
	// keeps detection and autonomy aligned (and follows the operator's edit).
	policyCfg := a.policyConfig(ctx)
	if class == ai.ClassHealthy || conf < policyCfg.MinConfidence {
		return
	}

	// incident memory: similar past incidents
	similar := a.similarPast(ctx, feats, deviceID)

	// policy
	var lastAuto map[string]time.Time
	a.mu.Lock()
	if t, ok := a.autoco[deviceID]; ok {
		lastAuto = map[string]time.Time{deviceID: t}
	}
	a.mu.Unlock()
	// The breaker counts the fleet *including* the device being decided. Our
	// own OPEN incident (if any) is excluded from the query so a single
	// already-alarming device does not count itself twice, then +1 accounts
	// for this decision: two devices alarming simultaneously reach the
	// configured threshold of 2.
	alarming := a.countAlarming(ctx, deviceID) + 1
	res := ai.Evaluate(ai.PolicyInput{
		DeviceID: deviceID, FaultClass: class, Confidence: conf,
		EtaAlarmMin: eta, AlarmingNow: alarming, LastAutoFor: lastAuto, Now: now,
	}, policyCfg, a.approvedClasses(ctx))

	// explainability: top-3 feature contributions to the winning class
	topFactors := a.model.TopFactors(feats, class, 3)

	// one span per AI decision — gives coldtrace-ai-agent its own node
	// and a queryable decision timeline in Datadog APM
	traceID, err := ai.DecisionSpan(ctx, a.tracer, ai.DecisionSpanInput{
		DeviceID: deviceID, FaultClass: class,
		Classification: string(res.Decision), Confidence: conf,
		EtaAlarmMin: eta, AlarmingNow: alarming, SimilarCount: len(similar),
		TopFactors: topFactors,
		Result:     res,
	})
	if err != nil {
		a.log.Warn("ai decision span failed", "err", err)
	}

	// dedupe at the DEVICE level: one OPEN incident per device. A physical
	// fault is one episode; the classifier's winning class wanders as its
	// window slides across the episode (straddling onset, full failure and
	// the cabinet pinned at ambient all read differently), and every wander
	// must not open a second, contradictory incident. This bit for real: one
	// injected compressor failure left three simultaneous OPEN incidents on
	// one device (compressor_failure, sensor_drift, power_outage), and
	// countAlarming() counts OPEN incidents — one dead freezer tripped the
	// fleet circuit breaker three times.
	var old incidentDoc
	errRefresh := a.inc.FindOne(ctx, bson.M{
		"deviceId": deviceID, "status": "OPEN"}).Decode(&old)
	if errRefresh == nil {
		if old.Class != class {
			// Reassessment of the same open episode. The incident keeps its
			// anchor — the first confident diagnosis — while the risk fields
			// (eta/confidence/decision) continue to refresh below.
			a.log.Info("ai_class_flip",
				"deviceId", deviceID, "from", old.Class, "to", class,
				"confidence", round3(conf))
		}
		// A breaker-held episode stays held for its whole life. The breaker
		// exists so a human reviews a correlated failure while the fleet is
		// compromised; letting the next tick auto-remediate because another
		// device recovered defeats it (observed live: FZ-005 flipped HOLD ->
		// AUTO when FZ-004 resolved).
		circuitLatched := old.CircuitBreaker || res.Circuit
		stickyCircuit := circuitLatched && old.RemediatedAt == nil
		// Once this episode has actually been auto-remediated, its decision is
		// history: later ticks see the device cooldown (or the fleet breaker
		// reacting to the incident itself) and would rewrite the card to HOLD,
		// claiming the agent refused while the command lifecycle shows ACKED.
		decision, reason := string(res.Decision), res.Reason
		if stickyCircuit {
			decision = string(ai.DecisionHold)
			reason = old.Reason
		}
		if old.RemediatedAt != nil {
			decision = string(ai.DecisionAuto)
			reason = "auto-remediated; awaiting recovery (repeat actions blocked by cooldown)"
		}
		decisionChanged := old.Decision != decision
		// etaAlarmMin NaN (no rising path) => store nil, never a NaN double
		var etaAny any
		if !math.IsNaN(eta) {
			etaAny = eta
		}
		if _, err := a.inc.UpdateOne(ctx, bson.M{"_id": old.ID},
			bson.M{"$set": bson.M{
				"etaAlarmMin": etaAny, "confidence": conf,
				"decision": decision, "reason": reason,
				"circuitBreaker": circuitLatched,
			}}); err != nil {
			a.log.Error("incident refresh failed", "err", err)
			return
		}
		if traceID != "" && old.TraceID == "" {
			_, _ = a.inc.UpdateOne(ctx, bson.M{"_id": old.ID},
				bson.M{"$set": bson.M{"traceId": traceID}})
		}
		// keep explainability fresh on the refreshed doc too
		if len(topFactors) > 0 && len(old.TopFactors) == 0 {
			_, _ = a.inc.UpdateOne(ctx, bson.M{"_id": old.ID},
				bson.M{"$set": bson.M{"topFactors": topFactors}})
		}
		// auto-remediate on the refreshed decision too — but never for a
		// breaker-held or already-remediated episode.
		if res.Decision == ai.DecisionAuto && !stickyCircuit && old.RemediatedAt == nil {
			if err := a.remediate(ctx, deviceID); err == nil {
				t := now
				_, _ = a.inc.UpdateOne(ctx, bson.M{"_id": old.ID}, bson.M{"$set": bson.M{
					"remediatedAt": t, "remediatedBy": "ai-agent"}})
				a.mu.Lock()
				a.autoco[deviceID] = now
				a.mu.Unlock()
			}
		}
		if !decisionChanged {
			return // same decision — nothing further to log
		}
		a.log.Info("ai_decision_refreshed",
			"deviceId", deviceID, "class", class, "confidence", round3(conf),
			"etaAlarmMin", round3(eta), "decision", decision, "reason", reason)
		return
	} else if !errors.Is(errRefresh, mongo.ErrNoDocuments) {
		a.log.Error("incident lookup failed", "err", errRefresh)
		return
	}

	doc := incidentDoc{
		DeviceID: deviceID, Class: class, Confidence: conf,
		Features: feats, Decision: string(res.Decision),
		Reason: res.Reason, Similar: similar, CircuitBreaker: res.Circuit,
		Status: "OPEN", OpenedAt: now,
		TraceID: traceID, TopFactors: topFactors,
	}
	// etaAlarmMin is NaN when the device is not on a rising path (e.g.
	// sensor_drift). NaN breaks json.Marshal ("unsupported value: NaN") —
	// which made GET /api/incidents return a silently-empty 200 — so store
	// nil instead (field is omitempty in both BSON and JSON tags).
	if !math.IsNaN(eta) {
		v := eta
		doc.EtaAlarmMin = &v
	}
	if res.Decision == ai.DecisionAuto {
		if err := a.remediate(ctx, deviceID); err == nil {
			t := now
			doc.RemediatedAt = &t
			doc.RemediatedBy = "ai-agent"
			a.mu.Lock()
			a.autoco[deviceID] = now
			a.mu.Unlock()
		} else {
			a.log.Error("auto remediation failed", "deviceId", deviceID, "err", err)
		}
	}
	ins, err := a.inc.InsertOne(ctx, doc)
	if err != nil {
		a.log.Error("incident persist failed", "err", err)
		return
	}
	a.log.Info("ai_decision",
		"incidentId", ins.InsertedID, "deviceId", deviceID,
		"class", class, "confidence", round3(conf), "etaAlarmMin", round3(eta),
		"decision", string(res.Decision), "reason", res.Reason,
		"similarCount", len(similar))

	// LLM root-cause analysis, best-effort in the background (never blocks
	// the decision path; incidents work fine without it).
	if a.llm.Enabled() {
		go a.analyzeRCA(ins.InsertedID, deviceID, class, conf, eta, feats)
	}
}

// analyzeRCA asks the OpenAI-compatible endpoint for a root-cause analysis of
// one incident and stores it on the incident document (bson "rca").
func (a *agent) analyzeRCA(id any, deviceID, class string, conf, eta float64, feats []float64) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	featStr := make([]string, len(feats))
	for i, f := range feats {
		featStr[i] = fmt.Sprintf("%.3f", f)
	}
	prompt := fmt.Sprintf(
		"Cold-chain freezer %s raised incident class=%s confidence=%.2f etaAlarmMin=%.1f. "+
			"Normalized 8-feature window [%s] (temp mean/std, slope, compressor duty, power, door events, range, slope accel). "+
			"Respond as strict JSON: {\"summary\":string,\"rootCause\":string,\"evidence\":[string],\"actions\":[string]}. "+
			"Be concise (<=2 sentences per field).",
		deviceID, class, conf, eta, strings.Join(featStr, ", "))

	out, usage, err := a.llm.ChatComplete(ctx, []ai.ChatMessage{
		{Role: "system", Content: "You are an industrial cold-chain SRE assistant. Answer only with the requested JSON."},
		{Role: "user", Content: prompt},
	})
	if err != nil {
		a.log.Warn("rca llm call failed", "deviceId", deviceID, "err", err)
		return
	}

	var rca IncidentRCA
	if err := json.Unmarshal([]byte(out), &rca); err != nil {
		// model wrapped the JSON or drifted — store raw text as summary instead
		rca = IncidentRCA{Summary: out, RootCause: "(raw model output — JSON parse failed)"}
	}
	rca.Model = a.llm.Model()
	rca.At = time.Now().UTC()
	if _, err := a.inc.UpdateOne(ctx, bson.M{"_id": id},
		bson.M{"$set": bson.M{"rca": rca}}); err != nil {
		a.log.Warn("rca persist failed", "deviceId", deviceID, "err", err)
		return
	}
	a.log.Info("ai_rca",
		"incidentId", id, "deviceId", deviceID, "model", rca.Model,
		"promptTokens", usage.PromptTokens, "completionTokens", usage.CompletionTokens)
}

// remediate issues the restart command with a FULL lifecycle: a commands
// doc (commandId, QUEUED->SENT) like the ops-api path, so the simulator's
// ack matches a real doc, the console shows ACKED with latency, and the
// audit trail keeps the AI action. (Previously the publish had no id: the
// ack came back with an empty commandId, nothing matched, UI showed
// TIMEOUT, and the AI action was invisible in the audit.)
func (a *agent) remediate(ctx context.Context, deviceID string) error {
	commandID := bson.NewObjectID().Hex()
	now := time.Now().UTC()
	cmd := map[string]any{
		"commandId": commandID, "deviceId": deviceID,
		"action": "compressor_restart", "status": "QUEUED",
		"issuedBy": "ai-agent", "createdAt": now, "expiresAt": now.Add(30 * time.Second),
	}
	if _, err := a.cmds.InsertOne(ctx, cmd); err != nil {
		return fmt.Errorf("command doc: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"id": commandID, "action": "compressor_restart", "issuedBy": "ai-agent", "at": now,
	})
	if _, err := a.mqtt.Publish(ctx, &paho.Publish{
		Topic: "coldchain/" + deviceID + "/commands", QoS: 1, Payload: payload,
	}); err != nil {
		if _, uerr := a.cmds.UpdateOne(ctx, bson.M{"commandId": commandID},
			bson.M{"$set": bson.M{"status": "FAILED", "ackDetail": "publish failed: " + err.Error()}}); uerr != nil {
			a.log.Warn("failed-command status update failed", "commandId", commandID, "err", uerr)
		}
		return err
	}
	if _, err := a.cmds.UpdateOne(ctx, bson.M{"commandId": commandID},
		bson.M{"$set": bson.M{"status": "SENT", "sentAt": time.Now().UTC()}}); err != nil {
		a.log.Warn("sent-status update failed", "commandId", commandID, "err", err)
	}
	return nil
}

// similarPast ranks past incidents by cosine similarity over their
// feature vectors (computed in-process, no search index).
func (a *agent) similarPast(ctx context.Context, feats []float64, excludeDevice string) []ai.SimilarMatch {
	return a.mem.TopSimilar(ctx, feats, excludeDevice, 3)
}

// countAlarming counts OTHER devices with an OPEN ALARM-class incident.
// NOTE: ai_incidents documents store the class under "faultClass".
//
// The caller adds 1 for the decision it is about to make, so the count is
// "how many devices are in ALARM once this one is counted". Excluding the
// current device matters on incident refresh: the device's own OPEN incident
// must count once, not twice.
func (a *agent) countAlarming(ctx context.Context, excludeDevice string) int {
	q := bson.M{"status": "OPEN", "faultClass": ai.ClassCompressorFailure}
	if excludeDevice != "" {
		q["deviceId"] = bson.M{"$ne": excludeDevice}
	}
	n, _ := a.inc.CountDocuments(ctx, q)
	return int(n)
}

// listIncidents GET /ai/incidents
func (a *agent) listIncidents(w http.ResponseWriter, r *http.Request) {
	cur, err := a.inc.Find(r.Context(), bson.D{}, options.Find().SetSort(bson.D{{Key: "openedAt", Value: -1}}).SetLimit(50))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	var docs []incidentDoc
	_ = cur.All(r.Context(), &docs)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(docs)
}

// getRCA GET /ai/incidents/{incidentID}/rca — the LLM root-cause analysis.
// 404 until the background analysis completes (or if LLM is unconfigured).
func (a *agent) getRCA(w http.ResponseWriter, r *http.Request) {
	id, err := bson.ObjectIDFromHex(chi.URLParam(r, "incidentID"))
	if err != nil {
		http.Error(w, "bad incident id", 400)
		return
	}
	var doc struct {
		RCA *IncidentRCA `bson:"rca" json:"rca"`
	}
	err = a.inc.FindOne(r.Context(), bson.M{"_id": id}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		http.Error(w, "incident not found", 404)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if doc.RCA == nil {
		http.Error(w, "rca not ready yet (analysis runs in background after the incident opens)", 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc.RCA)
}

// approve POST /ai/approvals/{faultClass} — human enables auto-remediation.
// Idempotent: one approval doc per class (upsert, not blind insert).
func (a *agent) approve(w http.ResponseWriter, r *http.Request) {
	class := chi.URLParam(r, "faultClass")
	_, err := a.approv.UpdateOne(r.Context(),
		bson.M{"faultClass": class},
		bson.M{
			"$set":         bson.M{"approvedAt": time.Now().UTC(), "approvedBy": "sre-console"},
			"$setOnInsert": bson.M{"faultClass": class},
		},
		options.UpdateOne().SetUpsert(true))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.log.Info("ai_class_approved", "faultClass", class)
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"approved":true}`))
}

// revoke DELETE /ai/approvals/{faultClass} — human disables auto-remediation.
func (a *agent) revoke(w http.ResponseWriter, r *http.Request) {
	class := chi.URLParam(r, "faultClass")
	res, err := a.approv.DeleteOne(r.Context(), bson.M{"faultClass": class})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if res.DeletedCount == 0 {
		http.Error(w, "class not approved", 404)
		return
	}
	a.log.Info("ai_class_revoked", "faultClass", class)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"revoked":true}`))
}

func (a *agent) listApprovals(w http.ResponseWriter, r *http.Request) {
	cur, err := a.approv.Find(r.Context(), bson.D{})
	if err != nil {
		http.Error(w, "approvals query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var docs []bson.M
	if err := cur.All(r.Context(), &docs); err != nil {
		http.Error(w, "approvals decode failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(docs)
}

// reset POST /ai/reset — demo cleanup hook. Clears the in-memory rolling
// windows, anomaly scorers, auto-remediation cooldowns and healthy streaks.
//
// Without this, a demo reset is only half a reset: the agent's 6-minute window
// still holds the fault that was just cleared, so it re-opens an incident
// seconds after /api/demo/reset resolved everything, and a rehearsed AUTO on a
// device keeps its 30-minute cooldown into the real judging run. New telemetry
// refills the windows from the reset instant onward.
func (a *agent) reset(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.wins = map[string]*ai.Window{}
	a.scorer = map[string]*ai.AnomalyScorer{}
	a.autoco = map[string]time.Time{}
	a.healthySince = map[string]time.Time{}
	a.policyCache = nil
	a.approvCache = nil
	a.mu.Unlock()
	a.log.Info("ai_state_reset", "detail", "windows, scorers, cooldowns and healthy streaks cleared")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"reset":true}`))
}

// feedback POST /ai/feedback/{incidentID} {resolvedClass: "..."} — resolves the
// incident and trains the model (auto-learning).
func (a *agent) feedback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ResolvedClass string `json:"resolvedClass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	id := chi.URLParam(r, "incidentID")
	oid, err := toObjectID(id)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	var doc incidentDoc
	if err := a.inc.FindOne(r.Context(), bson.M{"_id": oid}).Decode(&doc); err != nil {
		http.Error(w, "not found", 404)
		return
	}
	label := body.ResolvedClass
	if label == "" {
		label = doc.Class
	}
	_, _ = a.inc.UpdateOne(r.Context(), bson.M{"_id": oid},
		bson.M{"$set": bson.M{"status": "RESOLVED", "resolvedClass": label, "resolvedAt": time.Now().UTC()}})
	a.model.Learn(doc.Features, label)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"learned":true}`))
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
