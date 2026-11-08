// ColdTrace ops-api — the control-plane backend behind the Ops Console.
//
// Responsibilities:
//   - REST: fleet grid, device detail, telemetry series, events, incidents,
//     policy, approvals
//   - Command dispatch over MQTT with full lifecycle (QUEUED->SENT->ACKED,
//     latency measured) and ack-timeout sweeps
//   - SSE /api/stream: device twin changes + incidents + events via MongoDB
//     change streams (one fan-out hub)
//   - Demo control: inject faults / reset the fleet (drives the live demo)
//   - Light auth: OPS_TOKEN header when set (dev default: open)
//
// usage: OPS_PORT=8092 OPS_TOKEN=... MONGO_URI=... MQTT_BROKER=... ops-api
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
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
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/onemount/coldtrace/internal/aggdb"
	"github.com/onemount/coldtrace/internal/aiagent"
	"github.com/onemount/coldtrace/internal/modeleval"
	"github.com/onemount/coldtrace/internal/ops"
	"github.com/onemount/coldtrace/internal/otlp"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type server struct {
	log  *slog.Logger
	db   *mongo.Database
	mqtt *autopaho.ConnectionManager
	llm  *aiagent.Client

	// aiAgentURL is the in-network ai-agent base URL. Feedback must go through
	// it, not just be written to Mongo: Learn() lives in the agent's process
	// and retrains its in-memory classifier.
	aiAgentURL string

	hub *ops.Hub
}

func main() {
	baseLogger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "coldtrace-ops-api")
	ctx := context.Background()

	stopTrace, err := otlp.StartTrace(ctx, "coldtrace-ops-api")
	if err != nil {
		baseLogger.Warn("otlp trace init", "err", err)
	}
	stopMetric, err := otlp.StartMetric(ctx, "coldtrace-ops-api")
	if err != nil {
		baseLogger.Warn("otlp metric init", "err", err)
	}
	_, logProvider, err := otlp.StartLogs(ctx, "coldtrace-ops-api")
	if err != nil {
		baseLogger.Warn("otlp logs init", "err", err)
	}
	logger := otlp.NewTraceLogger(baseLogger, logProvider)
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

	mclient, err := mongo.Connect(options.Client().ApplyURI(env("MONGO_URI", "mongodb://mongodb:27017")).SetAppName("coldtrace-ops-api"))
	if err != nil {
		logger.Error("mongo connect", "err", err)
		os.Exit(1)
	}
	db := mclient.Database(env("MONGO_DB", "coldtrace"))
	store := ops.New(logger, db)

	u, err := url.Parse(env("MQTT_BROKER", "mqtt://broker:1883"))
	if err != nil {
		logger.Error("bad broker url", "err", err)
		os.Exit(1)
	}
	cfg := autopaho.ClientConfig{
		BrokerUrls:        []*url.URL{u},
		KeepAlive:         30,
		ConnectRetryDelay: 2 * time.Second,
		ClientConfig: paho.ClientConfig{
			ClientID:      "coldtrace-ops-api",
			OnClientError: func(err error) { logger.Warn("mqtt client error", "err", err) },
		},
	}
	srv := &server{log: logger, db: db, aiAgentURL: env("AI_AGENT_URL", "http://ai-agent:8091"), llm: aiagent.New(aiagent.Config{
		BaseURL: env("OPENAI_BASE_URL", ""),
		APIKey:  env("OPENAI_API_KEY", ""),
		Model:   env("OPENAI_MODEL", "gpt-4o-mini"),
		// Anthropic Messages API — wins over the OpenAI-compatible engine when set
		AnthropicBaseURL: env("ANTHROPIC_BASE_URL", ""),
		AnthropicAPIKey:  env("ANTHROPIC_API_KEY", ""),
		AnthropicModel:   env("ANTHROPIC_MODEL", "claude-sonnet-4-6"),
		// Cursor Cloud Agents engine — wins over every chat engine when set
		CursorAPIKey: env("CURSOR_API_KEY", ""),
		CursorAPIURL: env("CURSOR_API_URL", ""), // default https://api.cursor.com
		CursorModel:  env("CURSOR_MODEL", ""),   // default composer-2.5
		CursorRepo:   env("CURSOR_REPO", ""),    // repo the cloud agent runs against
		Timeout:      120 * time.Second,         // cloud agent runs take longer than chat
	}), hub: ops.NewHub(logger)}

	// device acks -> command lifecycle
	cfg.ClientConfig.Router = paho.NewStandardRouter()
	cfg.ClientConfig.Router.RegisterHandler("coldchain/+/acks", func(m *paho.Publish) {
		var ack struct {
			CommandID string    `json:"commandId"`
			OK        bool      `json:"ok"`
			Detail    string    `json:"detail"`
			At        time.Time `json:"at"`
		}
		if err := json.Unmarshal(m.Payload, &ack); err != nil {
			logger.Warn("bad ack payload", "topic", m.Topic, "err", err)
			return
		}
		if ack.At.IsZero() {
			ack.At = time.Now().UTC()
		}
		store.ApplyAck(ctx, ack.CommandID, ack.OK, ack.Detail, ack.At)
	})

	// Reconnect-safe acks subscription: OnConnectionUp fires on EVERY
	// (re)connection — including the first — so the subscription survives
	// broker resets. MQTT subscriptions are BROKER session state and every
	// autopaho reconnect negotiates a clean session: a one-shot Subscribe
	// after the first AwaitConnection died with the first TCP reset
	// (observed live 2026-11-08: a broker RST silently killed ack
	// consumption while publishes kept working, so every later command
	// rotted to TIMEOUT even though the simulator logged its acks).
	// gateway/simulator/fleet-simulator already used this shape; ops-api
	// was the gap. Regression test: TestAcksSubscriptionIsReconnectSafe.
	cfg.OnConnectionUp = func(cm *autopaho.ConnectionManager, _ *paho.Connack) {
		if _, err := cm.Subscribe(ctx, &paho.Subscribe{
			Subscriptions: []paho.SubscribeOptions{{Topic: "coldchain/+/acks", QoS: 1}},
		}); err != nil {
			logger.Error("acks subscribe failed", "err", err)
		}
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
	srv.mqtt = cm // acks are subscribed in OnConnectionUp (reconnect-safe)

	// change streams -> SSE hub (device_state + ai_incidents + events)
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	ops.WatchChangeStreams(streamCtx, logger, db, srv.hub)
	// ack timeout sweep every 5s
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			store.TimeoutStale(ctx)
		}
	}()

	// alert-rule evaluator every 30s (operator-authored thresholds -> events)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			store.EvaluateRules(ctx, logger)
		}
	}()

	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.Recoverer)
	// CORS before auth so browser preflight OPTIONS (no token header) is
	// answered 204 instead of being rejected with 401.
	r.Use(srv.cors)
	r.Use(srv.auth)
	// Long-horizon routes (AI agent chat stream) get a generous deadline;
	// everything else stays at the 30s default.
	r.Group(func(r chi.Router) {
		r.Use(chimw.Timeout(130 * time.Second))
		r.Post("/api/ai/chat", srv.postChat(store))
	})
	r.Group(func(r chi.Router) {
		r.Use(chimw.Timeout(30 * time.Second))
		srv.routes(r, store)
	})

	logger.Info("ops-api online", "port", env("OPS_PORT", "8092"), "llm", srv.llm.Enabled())
	httpSrv := &http.Server{
		Addr:              ":" + env("OPS_PORT", "8092"),
		Handler:           otelhttp.NewHandler(r, "coldtrace-ops-api"),
		ReadHeaderTimeout: 5 * time.Second,
	}
	// Graceful shutdown: on SIGTERM (docker compose stop) drain in-flight
	// requests, then let the deferred OTLP shutdown flush the last spans.
	// Without this, `docker compose down` SIGKILLs after the grace period and
	// the final seconds of every demo trace are missing from Datadog.
	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
		if err := httpSrv.Shutdown(ctx); err != nil {
			logger.Error("shutdown", "err", err)
		}
		cancel()
	}
}

// auth — light token check (header X-Operator-Token). Open when OPS_TOKEN
// unset. Browser preflight (OPTIONS) is always allowed so CORS negotiation
// works even with a token configured.
func (s *server) auth(next http.Handler) http.Handler {
	token := env("OPS_TOKEN", "")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if token != "" && r.Header.Get("X-Operator-Token") != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cors — permissive CORS for the ops console (dev/demo stack).
func (s *server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type,X-Operator-Token,X-Operator")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) routes(r chi.Router, store *ops.Store) {
	r.Route("/api", func(r chi.Router) {
		// fleet + device
		r.Get("/fleet", s.getFleet(store))
		r.Get("/devices/{deviceID}", s.getDevice(store))
		r.Get("/devices/{deviceID}/series", s.getSeries(store))
		// warm/cold tier history (telemetry_hourly | telemetry_daily) —
		// the READ side of the hot/warm/cold rollups
		r.Get("/devices/{deviceID}/history", s.getHistory(store))
		r.Post("/devices/{deviceID}/commands", s.postCommand(store))
		r.Patch("/devices/{deviceID}/desired", s.patchDesired(store))
		// events + incidents
		r.Get("/events", s.getEvents(store))
		r.Get("/incidents", s.getIncidents(store))
		r.Post("/incidents/{incidentID}/ack", s.ackIncident(store))
		r.Post("/incidents/{incidentID}/resolve", s.resolveIncident(store))
		r.Post("/incidents/{incidentID}/feedback", s.feedbackIncident(store))
		// policy + approvals
		r.Get("/policy", s.getPolicy(store))
		r.Put("/policy", s.putPolicy(store))
		r.Get("/approvals", s.getApprovals(store))
		r.Put("/approvals/{faultClass}", s.approveClass(store))
		r.Delete("/approvals/{faultClass}", s.revokeClass(store))
		// realtime
		r.Get("/stream", s.hub.ServeHTTP)
		// AI agent — RCA here (chat is registered in the long-timeout group)
		r.Post("/ai/rca/{incidentID}", s.postRCA(store))
		// model evaluation (accuracy + confusion matrix)
		r.Get("/model/eval", s.modelEval(store))
		// per-site rollup ($facet + $setWindowFields + $group showcase)
		r.Get("/insights/site", s.siteInsights(store))
		// alert rules (operator-authored thresholds)
		r.Get("/rules", s.listRules(store))
		r.Post("/rules", s.createRule(store))
		r.Delete("/rules/{ruleID}", s.deleteRule(store))
		// demo control
		r.Post("/demo/inject", s.postInject(store))
		r.Post("/demo/reset", s.postReset(store))
		// compliance (HACCP cold-chain audit): excursion minutes per device
		r.Get("/compliance/excursions.csv", s.excursionsCSV(store))
		// health
		r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"ok": true, "llm": s.llm.Enabled(), "engine": s.llm.Engine(), "at": time.Now().UTC()}, nil, s.log)
		})
	})
}

func writeJSON(w http.ResponseWriter, v any, err error, log *slog.Logger) {
	if err != nil {
		log.Error("handler", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	b, merr := json.Marshal(v)
	if merr != nil {
		// never emit a silent empty 200: surface the encoding failure
		log.Error("json marshal", "err", merr)
		http.Error(w, "response encoding failed: "+merr.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(b, '\n'))
}

func (s *server) getFleet(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := store.Fleet(r.Context())
		writeJSON(w, rows, err, s.log)
	}
}

func (s *server) getDevice(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "deviceID")
		var dev bson.M
		if err := store.Devices.FindOne(r.Context(), bson.M{"deviceId": id}).Decode(&dev); err != nil {
			if err == mongo.ErrNoDocuments {
				http.Error(w, "device not found", http.StatusNotFound)
				return
			}
			writeJSON(w, nil, err, s.log)
			return
		}
		var twin bson.M
		if err := store.Twin.FindOne(r.Context(), bson.M{"deviceId": id}).Decode(&twin); err == nil {
			dev["reported"] = twin["reported"]
			dev["lastSeen"] = twin["lastSeen"]
			dev["desired"] = twin["desired"]
		}
		writeJSON(w, dev, nil, s.log)
	}
}

func (s *server) getSeries(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "deviceID")
		window := 15
		if wv := r.URL.Query().Get("window"); wv != "" {
			fmt.Sscan(wv, &window)
		}
		if window < 1 || window > 1440 {
			http.Error(w, "window must be 1..1440 minutes", http.StatusBadRequest)
			return
		}
		coll := s.db.Collection("telemetry")
		rows, err := aggdb.DeviceSeries(r.Context(), coll, id, time.Duration(window)*time.Minute)
		writeJSON(w, rows, err, s.log)
	}
}

// getHistory — the warm/cold tier reader: per-device summary buckets from
// telemetry_hourly (7d retention) or telemetry_daily (365d), the data that
// outlives the 24h TTL on raw readings. Without this endpoint the tiering
// story was write-only: the rollup $merge'd summaries in and nothing ever
// read them back.
func (s *server) getHistory(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "deviceID")
		tierParam := r.URL.Query().Get("tier")
		if tierParam == "" {
			tierParam = string(aggdb.TierHourly)
		}
		tier := aggdb.Tier(tierParam)
		collName, err := tier.Collection()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		days := 7
		if dv := r.URL.Query().Get("days"); dv != "" {
			fmt.Sscan(dv, &days)
		}
		// Cap the ask at the tier's retention: beyond it the chart is not
		// "no data", it is "expired by design", and a silently-wrong range
		// would return a misleading empty history.
		maxDays := int(tier.MaxLookback() / (24 * time.Hour))
		if days < 1 || days > maxDays {
			http.Error(w, fmt.Sprintf("days must be 1..%d for tier %q (retention)", maxDays, tier), http.StatusBadRequest)
			return
		}
		rows, err := aggdb.TierHistory(r.Context(), s.db.Collection(collName), tier, id,
			time.Now().UTC().Add(-time.Duration(days)*24*time.Hour))
		writeJSON(w, rows, err, s.log)
	}
}

// postCommand — the operator control surface: QUEUED -> publish -> SENT.
func (s *server) postCommand(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceID := chi.URLParam(r, "deviceID")
		if !isDeviceID(deviceID) {
			http.Error(w, "invalid device id", http.StatusBadRequest)
			return
		}
		var body struct {
			Action string          `json:"action"`
			Args   json.RawMessage `json:"args,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Action == "" {
			http.Error(w, "action required", http.StatusBadRequest)
			return
		}
		if !allowedAction(body.Action) {
			http.Error(w, "action not allowed", http.StatusBadRequest)
			return
		}
		commandID := bson.NewObjectID().Hex()
		now := time.Now().UTC()
		args := bson.M{}
		if len(body.Args) > 0 {
			_ = json.Unmarshal(body.Args, &args)
		}
		cmd := ops.Command{
			CommandID: commandID, DeviceID: deviceID, Action: body.Action,
			Status: ops.CmdQueued, IssuedBy: "operator:" + operatorName(r),
			CreatedAt: now, ExpiresAt: now.Add(30 * time.Second),
		}
		if err := store.CreateCommand(r.Context(), &cmd); err != nil {
			writeJSON(w, nil, err, s.log)
			return
		}
		payload, _ := json.Marshal(map[string]any{
			"id": commandID, "action": body.Action, "args": args,
			"issuedBy": "operator", "at": now,
		})
		if _, err := s.mqtt.Publish(r.Context(), &paho.Publish{
			Topic:   "coldchain/" + deviceID + "/commands",
			QoS:     1,
			Payload: payload,
		}); err != nil {
			store.ApplyAck(r.Context(), commandID, false, "publish failed: "+err.Error(), time.Now().UTC())
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		store.MarkSent(r.Context(), commandID)
		store.AddEvent(r.Context(), ops.Event{
			Type: "command_SENT", DeviceID: deviceID,
			Message: body.Action + " dispatched", Actor: "operator:" + operatorName(r),
			CommandID: commandID, At: now,
		})
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"commandId": commandID, "status": ops.CmdSent})
	}
}

// patchDesired — flip the twin's desired state (setpoint change etc.).
func (s *server) patchDesired(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceID := chi.URLParam(r, "deviceID")
		if !isDeviceID(deviceID) {
			http.Error(w, "invalid device id", http.StatusBadRequest)
			return
		}
		var body struct {
			Setpoint *float64 `json:"setpoint"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		set := bson.M{}
		if body.Setpoint != nil {
			if *body.Setpoint > 0 || *body.Setpoint < -30 {
				http.Error(w, "setpoint must be in [-30,0]", http.StatusBadRequest)
				return
			}
			set["desired.setpoint"] = *body.Setpoint
			set["desired.updatedAt"] = time.Now().UTC()
			set["desired.updatedBy"] = operatorName(r)
		}
		if len(set) == 0 {
			http.Error(w, "nothing to update", http.StatusBadRequest)
			return
		}
		_, err := store.Twin.UpdateOne(r.Context(), bson.M{"deviceId": deviceID},
			bson.M{"$set": set}, options.UpdateOne().SetUpsert(true))
		writeJSON(w, map[string]any{"updated": err == nil}, err, s.log)
	}
}

func (s *server) getEvents(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := int64(100)
		if l := r.URL.Query().Get("limit"); l != "" {
			fmt.Sscan(l, &limit)
		}
		evs, err := store.ListEvents(r.Context(), r.URL.Query().Get("device"), r.URL.Query().Get("type"), limit)
		writeJSON(w, evs, err, s.log)
	}
}

func (s *server) getIncidents(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := bson.M{}
		if st := r.URL.Query().Get("status"); st != "" {
			q["status"] = st
		}
		cur, err := store.Incidents.Find(r.Context(), q,
			options.Find().SetSort(bson.D{{Key: "openedAt", Value: -1}}).SetLimit(100))
		if err != nil {
			s.log.Error("incidents find", "err", err)
			writeJSON(w, nil, err, s.log)
			return
		}
		rows, aerr := mustAll(r.Context(), cur)
		if aerr != nil {
			writeJSON(w, nil, aerr, s.log)
			return
		}
		s.log.Info("incidents_rows", "n", len(rows))
		writeJSON(w, rows, nil, s.log)
	}
}

func mustAll(ctx context.Context, cur *mongo.Cursor) ([]bson.M, error) {
	if cur == nil {
		return nil, fmt.Errorf("nil cursor")
	}
	var out []bson.M
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ackIncident — operator acknowledges an incident (ASSISTED workflow).
func (s *server) ackIncident(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := bson.ObjectIDFromHex(chi.URLParam(r, "incidentID"))
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		t := true
		_, err = store.Incidents.UpdateOne(r.Context(), bson.M{"_id": id},
			bson.M{"$set": bson.M{"acknowledged": t, "acknowledgedBy": operatorName(r), "acknowledgedAt": time.Now().UTC()}})
		if err != nil {
			writeJSON(w, nil, err, s.log)
			return
		}
		store.AddEvent(r.Context(), ops.Event{
			Type: "incident_ack", Actor: "operator:" + operatorName(r),
			IncidentID: id.Hex(), Message: "incident acknowledged", At: time.Now().UTC(),
		})
		writeJSON(w, map[string]any{"acked": true}, nil, s.log)
	}
}

func (s *server) resolveIncident(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := bson.ObjectIDFromHex(chi.URLParam(r, "incidentID"))
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		_, err = store.Incidents.UpdateOne(r.Context(), bson.M{"_id": id},
			bson.M{"$set": bson.M{"status": "RESOLVED", "resolvedAt": time.Now().UTC(), "resolvedBy": operatorName(r)}})
		if err != nil {
			writeJSON(w, nil, err, s.log)
			return
		}
		store.AddEvent(r.Context(), ops.Event{
			Type: "incident_resolved", Actor: "operator:" + operatorName(r),
			IncidentID: id.Hex(), Message: "incident resolved by operator", At: time.Now().UTC(),
		})
		writeJSON(w, map[string]any{"resolved": true}, nil, s.log)
	}
}

// feedbackIncident — resolution label that retrains the model.
func (s *server) feedbackIncident(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := bson.ObjectIDFromHex(chi.URLParam(r, "incidentID"))
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		var body struct {
			ResolvedClass string `json:"resolvedClass"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// Store the label first: the incident list must stay correct even if
		// the agent is briefly unreachable.
		set := bson.M{
			"status":        "RESOLVED",
			"resolvedAt":    time.Now().UTC(),
			"resolvedClass": body.ResolvedClass,
			"resolvedBy":    "operator:" + operatorName(r),
		}
		_, err = store.Incidents.UpdateOne(r.Context(), bson.M{"_id": id}, bson.M{"$set": set})
		if err != nil {
			writeJSON(w, nil, err, s.log)
			return
		}
		// Then hand it to the agent: Learn() lives in that process, so a Mongo
		// write alone would report success without changing the classifier.
		learned := s.forwardFeedback(r.Context(), id.Hex(), body.ResolvedClass)
		store.AddEvent(r.Context(), ops.Event{
			Type: "incident_feedback", Actor: "operator:" + operatorName(r),
			IncidentID: id.Hex(), Message: "resolved as " + body.ResolvedClass, At: time.Now().UTC(),
		})
		writeJSON(w, map[string]any{"learned": learned}, nil, s.log)
	}
}

// resetAgent clears the ai-agent's in-memory windows/cooldowns after a demo
// reset. Telemetry is not touched: the agent resumes from new readings.
func (s *server) resetAgent(ctx context.Context) {
	if s.aiAgentURL == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.aiAgentURL+"/ai/reset", nil)
	if err != nil {
		s.log.Warn("ai reset request", "err", err)
		return
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		s.log.Warn("ai reset forward failed", "err", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		s.log.Warn("ai reset rejected", "status", resp.StatusCode)
	}
}

// forwardFeedback posts an operator's resolution label to the ai-agent's
// /ai/feedback endpoint, which updates the incident and runs Learn().
func (s *server) forwardFeedback(ctx context.Context, incidentID, class string) bool {
	if s.aiAgentURL == "" {
		return false
	}
	payload, _ := json.Marshal(map[string]string{"resolvedClass": class})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.aiAgentURL+"/ai/feedback/"+url.PathEscape(incidentID), bytes.NewReader(payload))
	if err != nil {
		s.log.Warn("ai feedback request", "incidentId", incidentID, "err", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		s.log.Warn("ai feedback forward failed", "incidentId", incidentID, "err", err)
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		s.log.Warn("ai feedback rejected", "incidentId", incidentID, "status", resp.StatusCode)
		return false
	}
	return true
}

func (s *server) getPolicy(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var doc bson.M
		err := store.Policy.FindOne(r.Context(), bson.D{}).Decode(&doc)
		if err == mongo.ErrNoDocuments {
			doc = bson.M{"minConfidence": 0.85, "maxEtaAlarmMinutes": 45.0,
				"autoCooldownMinutes": 30, "fleetCircuitBreaker": 2, "killSwitch": false}
		}
		writeJSON(w, doc, nil, s.log)
	}
}

func (s *server) putPolicy(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		// whitelist editable fields
		allowed := map[string]bool{
			"minConfidence": true, "maxEtaAlarmMinutes": true, "autoCooldownMinutes": true,
			"fleetCircuitBreaker": true, "killSwitch": true,
		}
		set := bson.M{"updatedAt": time.Now().UTC(), "updatedBy": operatorName(r)}
		for k, v := range body {
			if allowed[k] {
				set[k] = v
			}
		}
		_, err := store.Policy.UpdateOne(r.Context(), bson.D{},
			bson.M{"$set": set, "$setOnInsert": bson.M{"createdAt": time.Now().UTC()}},
			options.UpdateOne().SetUpsert(true))
		s.storeEvent(store, r, "policy_update", fmt.Sprint(set))
		writeJSON(w, map[string]any{"updated": err == nil}, err, s.log)
	}
}

func (s *server) getApprovals(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cur, err := store.Approvals.Find(r.Context(), bson.D{})
		if err != nil {
			writeJSON(w, nil, err, s.log)
			return
		}
		rows, aerr := mustAll(r.Context(), cur)
		writeJSON(w, rows, aerr, s.log)
	}
}

func (s *server) approveClass(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		class := chi.URLParam(r, "faultClass")
		_, err := store.Approvals.UpdateOne(r.Context(), bson.M{"faultClass": class},
			bson.M{"$set": bson.M{"approvedAt": time.Now().UTC(), "approvedBy": operatorName(r)},
				"$setOnInsert": bson.M{"faultClass": class}},
			options.UpdateOne().SetUpsert(true))
		if err != nil {
			writeJSON(w, nil, err, s.log)
			return
		}
		s.storeEvent(store, r, "approval_granted", "auto-remediation approved for class "+class)
		writeJSON(w, map[string]any{"approved": true}, nil, s.log)
	}
}

func (s *server) revokeClass(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		class := chi.URLParam(r, "faultClass")
		res, err := store.Approvals.DeleteOne(r.Context(), bson.M{"faultClass": class})
		if err != nil {
			writeJSON(w, nil, err, s.log)
			return
		}
		s.storeEvent(store, r, "approval_revoked", "auto-remediation revoked for class "+class)
		writeJSON(w, map[string]any{"revoked": res.DeletedCount > 0}, nil, s.log)
	}
}

func (s *server) postRCA(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := bson.ObjectIDFromHex(chi.URLParam(r, "incidentID"))
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		rca, err := s.llm.RootCause(r.Context(), s.db, store, id)
		if err != nil {
			writeJSON(w, nil, err, s.log)
			return
		}
		writeJSON(w, rca, nil, s.log)
	}
}

func (s *server) postChat(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []aiagent.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) == 0 {
			http.Error(w, "messages required", http.StatusBadRequest)
			return
		}
		s.hub.SSEHeaders(w)
		flusher := s.hub.Flusher(w)
		// Grounded agentic chat: the model may pull live fleet/incident facts
		// via read-only tools before answering. Template fallback on failure.
		err := s.llm.GroundedChat(r.Context(), s.db, body.Messages, func(delta string) {
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"delta": delta}))
			flusher.Flush()
		})
		if err != nil {
			s.log.Warn("ai chat", "err", err)
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"error": err.Error()}))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

// postInject — demo control: inject a fault on a device (or all devices).
func (s *server) postInject(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DeviceID string `json:"deviceId"`
			Fault    string `json:"fault"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Fault == "" {
			http.Error(w, "fault required", http.StatusBadRequest)
			return
		}
		if body.DeviceID == "" {
			body.DeviceID = "FZ-003" // demo default
		}
		if !isDeviceID(body.DeviceID) {
			http.Error(w, "invalid device id", http.StatusBadRequest)
			return
		}
		commandID := bson.NewObjectID().Hex()
		now := time.Now().UTC()
		cmd := ops.Command{
			CommandID: commandID, DeviceID: body.DeviceID, Action: "set_fault",
			Args: bson.M{"fault": body.Fault}, Status: ops.CmdQueued,
			IssuedBy: "operator:" + operatorName(r), CreatedAt: now, ExpiresAt: now.Add(30 * time.Second),
		}
		if err := store.CreateCommand(r.Context(), &cmd); err != nil {
			writeJSON(w, nil, err, s.log)
			return
		}
		payload, _ := json.Marshal(map[string]any{
			"id": commandID, "action": "set_fault",
			"args": map[string]any{"fault": body.Fault}, "issuedBy": "demo", "at": now,
		})
		if _, err := s.mqtt.Publish(r.Context(), &paho.Publish{
			Topic:   "coldchain/" + body.DeviceID + "/commands",
			QoS:     1,
			Payload: payload,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		store.MarkSent(r.Context(), commandID)
		store.AddEvent(r.Context(), ops.Event{
			Type: "fault_injected", DeviceID: body.DeviceID,
			Message: "demo fault injected: " + body.Fault, Actor: "operator:" + operatorName(r),
			CommandID: commandID, Severity: "warning", At: now,
		})
		writeJSON(w, map[string]any{"commandId": commandID, "injected": body.Fault, "device": body.DeviceID}, nil, s.log)
	}
}

// postReset — demo control: clear every fault in the fleet.
func (s *server) postReset(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		commandID := bson.NewObjectID().Hex()
		now := time.Now().UTC()
		cmd := ops.Command{
			CommandID: commandID, DeviceID: "FLEET", Action: "demo_reset",
			Status: ops.CmdQueued, IssuedBy: "operator:" + operatorName(r), CreatedAt: now, ExpiresAt: now.Add(30 * time.Second),
		}
		_ = store.CreateCommand(r.Context(), &cmd)
		payload, _ := json.Marshal(map[string]any{
			"id": commandID, "action": "demo_reset", "issuedBy": "demo", "at": now,
		})
		// broadcast on a fleet-wide topic; the simulator restores every device
		// (fault cleared, door closed, sensor calibrated, cabinet at setpoint)
		if _, err := s.mqtt.Publish(r.Context(), &paho.Publish{
			Topic:   "coldchain/fleet/commands",
			QoS:     1,
			Payload: payload,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		store.MarkSent(r.Context(), commandID)
		// A fleet-wide broadcast has no per-device ack to wait for; mark it
		// ACKED at publish time instead of letting the expiry sweeper emit a
		// spurious command_TIMEOUT on every demo reset.
		store.ApplyAck(r.Context(), commandID, true, "fleet broadcast accepted", now)
		// full demo reset: clear desired overrides + resolve lingering OPEN
		// incidents so a fresh judging run starts pristine
		store.Twin.UpdateMany(r.Context(), bson.M{},
			bson.M{"$set": bson.M{"desired.setpoint": -18, "desired.updatedAt": now, "desired.updatedBy": "system:reset"}})
		store.Incidents.UpdateMany(r.Context(), bson.M{"status": "OPEN"},
			bson.M{"$set": bson.M{"status": "RESOLVED", "resolvedAt": now, "resolvedBy": "system:reset"}})
		// The agent's 6-minute rolling windows still hold the fault we just
		// cleared, so without this it re-opens an incident seconds later. Its
		// per-device cooldowns also have to die with the rehearsal.
		s.resetAgent(r.Context())
		store.AddEvent(r.Context(), ops.Event{
			Type: "demo_reset", Message: "demo fleet reset issued (faults cleared, incidents resolved)",
			Actor: "operator:" + operatorName(r), CommandID: commandID, At: now,
		})
		writeJSON(w, map[string]any{"commandId": commandID, "incidentsResolved": true}, nil, s.log)
	}
}

// siteInsights — per-site rollup for the console "Sites" strip.
func (s *server) siteInsights(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := store.SiteInsights(r.Context())
		writeJSON(w, rows, err, s.log)
	}
}

// listRules — GET /api/rules
func (s *server) listRules(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rules, err := store.ListRules(r.Context())
		writeJSON(w, rules, err, s.log)
	}
}

// createRule — POST /api/rules {metric, op, threshold, severity, deviceId?}
func (s *server) createRule(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DeviceID  string  `json:"deviceId"`
			Metric    string  `json:"metric"`
			Op        string  `json:"op"`
			Threshold float64 `json:"threshold"`
			Severity  string  `json:"severity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		rule := ops.Rule{
			RuleID:    bson.NewObjectID().Hex()[:12],
			DeviceID:  body.DeviceID,
			Metric:    body.Metric,
			Op:        body.Op,
			Threshold: body.Threshold,
			Severity:  body.Severity,
			Enabled:   true,
			CreatedBy: "operator:" + operatorName(r),
			CreatedAt: time.Now().UTC(),
		}
		if err := ops.ValidRule(&rule); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := store.CreateRule(r.Context(), &rule); err != nil {
			writeJSON(w, nil, err, s.log)
			return
		}
		store.AddEvent(r.Context(), ops.Event{
			Type: "rule_created", DeviceID: rule.DeviceID,
			Message: fmt.Sprintf("alert rule %s: %s %s %.2f (%s)",
				rule.RuleID, rule.Metric, rule.Op, rule.Threshold, rule.Severity),
			Actor: "operator:" + operatorName(r), At: time.Now().UTC(),
		})
		writeJSON(w, rule, nil, s.log)
	}
}

// deleteRule — DELETE /api/rules/{ruleID}
func (s *server) deleteRule(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ruleID := chi.URLParam(r, "ruleID")
		if err := store.DeleteRule(r.Context(), ruleID); err != nil {
			writeJSON(w, nil, err, s.log)
			return
		}
		s.storeEvent(store, r, "rule_deleted", "alert rule "+ruleID+" deleted")
		writeJSON(w, map[string]any{"deleted": true}, nil, s.log)
	}
}

// modelEval — offline accuracy report for the fault classifier (shared logic
// with tools/evalmodel). Expensive (~seconds of warm-start), so the report
// is computed once and cached.
func (s *server) modelEval(store *ops.Store) http.HandlerFunc {
	var once sync.Once
	var report modeleval.Report
	return func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { report = modeleval.Evaluate(220) })
		writeJSON(w, report, nil, s.log)
	}
}

// excursionsCSV — HACCP-style cold-chain audit: minutes each device spent
// above the -14°C at-risk threshold, plus worst reading, last 24h, and the
// LAST excursion instant in LOCAL time. Served as CSV for food-safety
// auditors (the "beyond the dashboard" story). The pipeline lives in
// internal/ops (ExcursionsPipeline) with tests pinning its shape; the
// tz-formatted column is the brief's "explicit timezone on date operators"
// rule applied to the export an inspector actually reads.
func (s *server) excursionsCSV(store *ops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		coll := s.db.Collection("telemetry")
		since := time.Now().UTC().Add(-24 * time.Hour)
		tz := env("AUDIT_TZ", aggdb.VietnamTimezone)
		cur, err := coll.Aggregate(r.Context(), ops.ExcursionsPipeline(since, tz))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition",
			`attachment; filename="coldtrace-excursions-`+time.Now().UTC().Format("20060102")+`.csv"`)
		_, _ = fmt.Fprintf(w, "deviceId,excursionMinutesAboveMinus14C,worstTempC,samplesAboveThreshold,lastExcursionLocal,windowHours\n")
		for cur.Next(r.Context()) {
			var row ops.ExcursionRow
			if err := cur.Decode(&row); err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "%s,%.1f,%.2f,%d,%s,24\n",
				row.ID, row.ExcursionMin, row.WorstTempC, row.Samples, row.LastExcursionLocal)
		}
	}
}

func (s *server) storeEvent(store *ops.Store, r *http.Request, typ, msg string) {
	store.AddEvent(r.Context(), ops.Event{
		Type: typ, Message: msg, Actor: "operator:" + operatorName(r), At: time.Now().UTC(),
	})
}

func operatorName(r *http.Request) string {
	if n := r.Header.Get("X-Operator"); n != "" {
		return n
	}
	return "anonymous"
}

func isDeviceID(id string) bool {
	if len(id) != 6 || id[2] != '-' {
		return false
	}
	for i, c := range id {
		switch {
		case i < 2:
			if c < 'A' || c > 'Z' {
				return false
			}
		case i == 2:
		default:
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

func allowedAction(a string) bool {
	switch a {
	case "compressor_restart", "defrost", "reboot", "set_setpoint":
		return true
	}
	return false
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
