package main

// Regression coverage for the incident dedup scope in evaluate(): one OPEN
// incident per DEVICE, not per device+class.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/onemount/coldtrace/internal/ai"
	"github.com/onemount/coldtrace/internal/sim"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel/trace"
)

// faultWindow drives the real device simulator through a warm-up then the
// fault and returns the window the live loop would evaluate (last 72 ticks).
func faultWindow(t *testing.T, log *slog.Logger, fault string, seed int64, back time.Duration) []ai.Sample {
	t.Helper()
	d := sim.NewSeeded("FZ", log, seed)
	for i := 0; i < 120; i++ {
		d.Tick()
	}
	d.SetFault(fault)
	w := ai.NewWindow("dedup", 72)
	ts := time.Now().UTC().Add(-back).Add(-6 * time.Minute)
	for i := 0; i < 72; i++ {
		r := d.Tick()
		ts = ts.Add(5 * time.Second)
		w.Push(ai.Sample{TS: ts, TempC: r.TempC, PowerDraw: r.PowerDraw,
			CompressorRunning: r.CompressorRunning, DoorOpen: r.DoorOpen})
	}
	return w.Samples
}

// TestEvaluateOneOpenIncidentPerDevice pins the dedup scope.
//
// This bit for real on the demo fleet: one injected compressor failure left
// THREE simultaneous OPEN incidents on one device (compressor_failure 0.96,
// sensor_drift 1.00, power_outage 0.92) because the classifier's winning
// class wanders as its window slides across the episode — and the old dedup
// keyed on device+class, so every wander opened a new, contradictory
// incident. countAlarming() counts OPEN compressor_failure incidents, so one
// dead freezer also tripped the fleet circuit breaker three times.
//
// The incident list must show one anchored episode: the first confident
// diagnosis stands, later reassessments refresh risk fields in place.
func TestEvaluateOneOpenIncidentPerDevice(t *testing.T) {
	ctx := context.Background()
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		uri = "mongodb://localhost:27017"
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetServerSelectionTimeout(2 * time.Second))
	if err != nil {
		t.Skipf("mongo connect: %v", err)
	}
	defer func() { _ = client.Disconnect(ctx) }()
	if err := client.Ping(ctx, nil); err != nil {
		t.Skipf("no MongoDB at %s: %v", uri, err)
	}

	db := client.Database("coldtrace_agent_dedup_test")
	if err := db.Drop(ctx); err != nil {
		t.Fatalf("drop test db: %v", err)
	}
	defer func() { _ = db.Drop(ctx) }()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &agent{
		log:          log,
		db:           db,
		coll:         db.Collection("telemetry"),
		inc:          db.Collection("ai_incidents"),
		approv:       db.Collection("ai_approvals"),
		cmds:         db.Collection("commands"),
		model:        ai.NewClassifier(log),
		wins:         map[string]*ai.Window{},
		scorer:       map[string]*ai.AnomalyScorer{},
		autoco:       map[string]time.Time{},
		healthySince: map[string]time.Time{},
		approvCache:  map[string]bool{},
		tracer:       trace.NewNoopTracerProvider().Tracer("test"),
		mem:          ai.NewIncidentMemory(db.Collection("ai_incidents")),
	}

	openIncidents := func() []incidentDoc {
		cur, err := a.inc.Find(ctx, bson.M{"deviceId": "FZ-T01", "status": "OPEN"})
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		var docs []incidentDoc
		if err := cur.All(ctx, &docs); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return docs
	}

	// Episode 1: compressor failure detected — one incident opens.
	a.evaluate(ctx, "FZ-T01", faultWindow(t, log, sim.FaultCompressorFailure, 41, 10*time.Minute), time.Now())
	open := openIncidents()
	if len(open) != 1 {
		t.Fatalf("after the first evaluation: %d OPEN incidents, want 1", len(open))
	}
	anchor := open[0]
	if anchor.Class == ai.ClassHealthy {
		t.Fatalf("a healthy classification must not open an incident at all")
	}

	// Episode 2: the SAME device, later in the SAME outage, its window now
	// reading like a power failure (compressor off, no power, cabinet warm).
	// The classifier may change its mind; the incident list may not grow a
	// second, contradictory entry.
	a.evaluate(ctx, "FZ-T01", faultWindow(t, log, sim.FaultPowerOutage, 42, 10*time.Minute), time.Now().Add(2*time.Hour))
	open = openIncidents()
	if len(open) != 1 {
		classes := make([]string, len(open))
		for i, d := range open {
			classes[i] = d.Class
		}
		t.Fatalf("after reassessment: %d OPEN incidents (%v), want the single anchored episode", len(open), classes)
	}
	if open[0].Class != anchor.Class {
		t.Fatalf("incident class = %s, want the anchor %s: the first confident diagnosis must survive reassessment",
			open[0].Class, anchor.Class)
	}
	if open[0].ID.Hex() != anchor.ID.Hex() {
		t.Fatalf("reassessment must refresh the existing incident in place, not replace it")
	}
}
