package main

// Agentic failure handling — regression coverage for checkRemediationFailure:
// a remediated episode whose auto-restart the DEVICE refused (command
// status FAILED) must be escalated to on-call after two refusals, with a
// pre-drafted context note and a critical event in the shared audit log.
// One refusal inside the backoff window must NOT retry yet (the retry
// would need a live MQTT connection — the gate is what keeps this
// testable without a broker), and an ACKED restart must no-op.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// failureTestAgent builds the minimal agent against its own test DB
// (skip-without-Mongo, same convention as agent_dedup_test.go). mqtt is nil
// on purpose: none of the paths under test may reach it.
func failureTestAgent(t *testing.T) (*agent, context.Context) {
	t.Helper()
	ctx := context.Background()
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		uri = "mongodb://localhost:27017"
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetServerSelectionTimeout(2 * time.Second))
	if err != nil {
		t.Skipf("mongo connect: %v", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		t.Skipf("no MongoDB at %s: %v", uri, err)
	}
	t.Cleanup(func() { _ = client.Disconnect(ctx) })
	db := client.Database("coldtrace_agent_failure_test")
	if err := db.Drop(ctx); err != nil {
		t.Fatalf("drop test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Drop(ctx) })
	return &agent{
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:   db,
		inc:  db.Collection("ai_incidents"),
		cmds: db.Collection("commands"),
		evs:  db.Collection("events"),
	}, ctx
}

func seedIncident(t *testing.T, a *agent, ctx context.Context) *incidentDoc {
	t.Helper()
	now := time.Now().UTC()
	eta := 44.0
	inc := &incidentDoc{
		DeviceID: "FZ-T", Class: "compressor_failure", Confidence: 0.95,
		Decision: "AUTO", Status: "OPEN", OpenedAt: now.Add(-5 * time.Minute),
		EtaAlarmMin: &eta, RemediatedAt: &now, RemediatedBy: "ai-agent",
	}
	res, err := a.inc.InsertOne(ctx, inc)
	if err != nil {
		t.Fatalf("seed incident: %v", err)
	}
	inc.ID = res.InsertedID.(bson.ObjectID)
	return inc
}

func seedCommand(a *agent, ctx context.Context, status string, age time.Duration) {
	t := time.Now().UTC().Add(-age)
	_, _ = a.cmds.InsertOne(ctx, bson.M{
		"commandId": bson.NewObjectID().Hex(), "deviceId": "FZ-T",
		"action": "compressor_restart", "issuedBy": "ai-agent", "status": status,
		"ackDetail": "restart failed: thermal overload latched — power-cycle the controller (reboot) to reset",
		"createdAt": t,
	})
}

// TestCheckRemediationFailureEscalates: two refused restarts -> the episode
// is escalated exactly once, with the pre-drafted on-call note (manual
// action included) and a critical incident_escalated event.
func TestCheckRemediationFailureEscalates(t *testing.T) {
	a, ctx := failureTestAgent(t)
	inc := seedIncident(t, a, ctx)
	now := time.Now().UTC()
	seedCommand(a, ctx, "FAILED", 2*time.Minute)
	seedCommand(a, ctx, "FAILED", 1*time.Minute)

	a.checkRemediationFailure(ctx, inc, now)

	var got incidentDoc
	if err := a.inc.FindOne(ctx, bson.M{"_id": inc.ID}).Decode(&got); err != nil {
		t.Fatalf("reload incident: %v", err)
	}
	if !got.Escalated || got.EscalatedAt == nil {
		t.Fatalf("incident must be escalated after 2 refusals, got escalated=%v", got.Escalated)
	}
	for _, want := range []string{"compressor_failure", "reboot", "thermal overload", "FAILED 2x"} {
		if !strings.Contains(got.EscalationNote, want) {
			t.Errorf("escalationNote missing %q: %s", want, got.EscalationNote)
		}
	}
	n, err := a.evs.CountDocuments(ctx, bson.M{"type": "incident_escalated", "severity": "critical"})
	if err != nil || n != 1 {
		t.Fatalf("want exactly 1 critical incident_escalated event, got %d (err %v)", n, err)
	}

	// idempotent: the Escalated latch stops a second escalation
	a.checkRemediationFailure(ctx, &got, now)
	n, _ = a.evs.CountDocuments(ctx, bson.M{"type": "incident_escalated"})
	if n != 1 {
		t.Fatalf("escalation must latch (no double-page), events=%d", n)
	}
}

// TestCheckRemediationFailureBackoffGate: a single refusal INSIDE the
// backoff window must neither escalate nor retry (the retry path needs a
// live broker; the gate is what makes this deterministic). Past the
// backoff it still only retries once — never a third command.
func TestCheckRemediationFailureBackoffGate(t *testing.T) {
	a, ctx := failureTestAgent(t)
	inc := seedIncident(t, a, ctx)
	seedCommand(a, ctx, "FAILED", 5*time.Second) // fresh refusal

	a.checkRemediationFailure(ctx, inc, time.Now().UTC())

	var got incidentDoc
	_ = a.inc.FindOne(ctx, bson.M{"_id": inc.ID}).Decode(&got)
	if got.Escalated {
		t.Fatal("one refusal must not escalate")
	}
	n, _ := a.cmds.CountDocuments(ctx, bson.M{"deviceId": "FZ-T"})
	if n != 1 {
		t.Fatalf("backoff gate must block the immediate retry, commands=%d", n)
	}
}

// TestCheckRemediationFailureIgnoresAcked: an ACKED restart means the fix
// landed — the watcher must no-op entirely (no escalation, no events).
func TestCheckRemediationFailureIgnoresAcked(t *testing.T) {
	a, ctx := failureTestAgent(t)
	inc := seedIncident(t, a, ctx)
	seedCommand(a, ctx, "ACKED", time.Minute)

	a.checkRemediationFailure(ctx, inc, time.Now().UTC())

	var got incidentDoc
	_ = a.inc.FindOne(ctx, bson.M{"_id": inc.ID}).Decode(&got)
	if got.Escalated {
		t.Fatal("an ACKED restart must never escalate")
	}
	n, _ := a.evs.CountDocuments(ctx, bson.M{})
	if n != 0 {
		t.Fatalf("no events expected for an ACKED restart, got %d", n)
	}
}
