package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/onemount/coldtrace/internal/aggdb"
	"github.com/onemount/coldtrace/internal/ingest"
	"github.com/onemount/coldtrace/internal/otlp"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// deviceIDPattern — fleet device IDs look like FZ-001. Anything else never
// reaches an MQTT topic (topic injection guard).
var deviceIDPattern = regexp.MustCompile(`^[A-Z]{2}-\d{3}$`)

func main() {
	broker := flag.String("broker", env("MQTT_BROKER", "mqtt://broker:1883"), "mqtt broker url")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "coldtrace-gateway")
	ctx := context.Background()

	// OTLP traces + metrics + logs (local otel-collector → Datadog).
	stopTrace, err := otlp.StartTrace(ctx, "coldtrace-gateway")
	if err != nil {
		logger.Warn("otlp trace init", "err", err)
	}
	stopMetric, err := otlp.StartMetric(ctx, "coldtrace-gateway")
	if err != nil {
		logger.Warn("otlp metric init", "err", err)
	}
	_, logProvider, err := otlp.StartLogs(ctx, "coldtrace-gateway")
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

	mclient, err := mongo.Connect(options.Client().ApplyURI(env("MONGO_URI", "mongodb://mongodb:27017")).SetAppName("coldtrace-gateway"))
	if err != nil {
		logger.Error("mongo connect", "err", err)
		os.Exit(1)
	}
	// fail fast with a clear message instead of timing out per-request later
	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pingCancel()
	if err := mclient.Ping(pingCtx, nil); err != nil {
		logger.Error("mongo ping failed (is MONGO_URI reachable?)", "err", err)
		os.Exit(1)
	}
	db := mclient.Database(env("MONGO_DB", "coldtrace"))
	coll := db.Collection("telemetry")
	events := db.Collection("events")          // audit trail: commands, dispatches, acks
	incidents := db.Collection("ai_incidents") // read-only here; the ai-agent writes incidents

	// Registry join: the telemetry metaField carries {deviceId, site, zone}
	// so site/zone filters can skip whole buckets (time series brief, p.11).
	ing := ingest.NewWithTwin(logger, coll,
		db.Collection("device_state"), db.Collection("events")).
		WithDevices(db.Collection("devices"))

	// Batched writes: insertMany batches of up to 100 readings or 250 ms,
	// { ordered: false }, bounded exponential-backoff retries. This is the
	// workshop brief's recommended write path for time series data.
	batcher := ing.EnableBatching(ingest.DefaultBatchConfig(), logger)
	shutdownBatcher := func() {
		// Drain the queue so a clean stop does not lose buffered readings.
		done := make(chan struct{})
		go func() { batcher.Close(); close(done) }()
		select {
		case <-done:
			logger.Info("gateway_batcher_drained")
		case <-time.After(10 * time.Second):
			logger.Warn("gateway_batcher_drain_timeout")
		}
	}

	u, err := url.Parse(*broker)
	if err != nil {
		logger.Error("bad broker url", "err", err)
		os.Exit(1)
	}

	cfg := autopaho.ClientConfig{
		BrokerUrls:        []*url.URL{u},
		KeepAlive:         30,
		ConnectRetryDelay: 2 * time.Second,
		ClientConfig: paho.ClientConfig{
			ClientID: "coldtrace-gateway",
			OnServerDisconnect: func(*paho.Disconnect) {
				logger.Warn("broker disconnected; retrying")
			},
			OnClientError: func(err error) { logger.Warn("mqtt client error", "err", err) },
		},
	}
	cfg.ClientConfig.Router = paho.NewSingleHandlerRouter(ing.HandlePublish)
	cfg.OnConnectionUp = func(cm *autopaho.ConnectionManager, _ *paho.Connack) {
		if _, err := cm.Subscribe(ctx, &paho.Subscribe{
			Subscriptions: []paho.SubscribeOptions{
				{Topic: "coldchain/+/telemetry", QoS: 1},
				{Topic: "coldchain/+/acks", QoS: 1},
			},
		}); err != nil {
			logger.Error("subscribe failed", "err", err)
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

	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.Recoverer)
	r.Get("/devices/health", func(w http.ResponseWriter, req *http.Request) {
		rows, err := aggdb.DeviceHealth(req.Context(), coll, "")
		writeJSON(w, rows, err, logger)
	})
	r.Get("/devices/health/{deviceID}", func(w http.ResponseWriter, req *http.Request) {
		rows, err := aggdb.DeviceHealth(req.Context(), coll, chi.URLParam(req, "deviceID"))
		writeJSON(w, rows, err, logger)
	})
	r.Get("/fleet/status", func(w http.ResponseWriter, req *http.Request) {
		rows, err := aggdb.FleetRollup(req.Context(), coll)
		writeJSON(w, rows, err, logger)
	})
	r.Get("/incidents", func(w http.ResponseWriter, req *http.Request) {
		cur, err := incidents.Find(req.Context(), bson.D{}, options.Find().SetSort(bson.D{{Key: "openedAt", Value: -1}}).SetLimit(100))
		if err != nil {
			writeJSON(w, nil, err, logger)
			return
		}
		var docs []bson.M
		if err := cur.All(req.Context(), &docs); err != nil {
			writeJSON(w, nil, err, logger)
			return
		}
		writeJSON(w, docs, nil, logger)
	})
	r.Post("/devices/{deviceID}/remediate", func(w http.ResponseWriter, req *http.Request) {
		deviceID := chi.URLParam(req, "deviceID")
		if !deviceIDPattern.MatchString(deviceID) {
			http.Error(w, "invalid device id", http.StatusBadRequest)
			return
		}
		// The id is what the device echoes back as commandId, and what
		// store.ApplyAck matches on to flip the command from SENT to ACKED.
		// Omitting it (as this handler used to) leaves every gateway-issued
		// remediation stuck at SENT forever.
		commandID := bson.NewObjectID().Hex()
		payload, _ := json.Marshal(map[string]any{
			"id": commandID, "action": "compressor_restart",
			"issuedBy": "sre-console", "at": time.Now().UTC(),
		})
		if _, err := cm.Publish(req.Context(), &paho.Publish{
			Topic: "coldchain/" + deviceID + "/commands", QoS: 1, Payload: payload,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		// Record the dispatch in the `events` audit trail, NOT in
		// `ai_incidents`. A manual remediation is an action taken against an
		// incident, not an incident itself: writing it to ai_incidents
		// produced a record with openedAt == resolvedAt, no confidence and no
		// evidence, which then showed up in the incident list and the model's
		// history as a zero-duration "AUTO" resolution.
		now := time.Now().UTC()
		if _, err := events.InsertOne(req.Context(), bson.M{
			"ts":           now,
			"type":         "technician_dispatched",
			"deviceId":     deviceID,
			"commandId":    commandID,
			"action":       "compressor_restart",
			"issuedBy":     "sre-console",
			"remediatedBy": "sre-console",
		}); err != nil {
			logger.Error("dispatch record failed", "err", err)
		}
		logger.Info("technician_dispatched", "deviceId", deviceID, "command", "compressor_restart")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"queued":true}`))
	})

	logger.Info("gateway online", "broker", *broker)
	srv := &http.Server{
		Addr:              ":" + env("GATEWAY_PORT", "8080"),
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server", "err", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logger.Info("gateway_shutdown", "reason", "signal")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	shutdownBatcher()
}

func writeJSON(w http.ResponseWriter, v any, err error, log *slog.Logger) {
	if err != nil {
		log.Error("handler", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	b, merr := json.Marshal(v)
	if merr != nil {
		log.Error("json marshal", "err", merr)
		http.Error(w, "response encoding failed: "+merr.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(b, '\n'))
}
