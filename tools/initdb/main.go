// initdb creates the ColdTrace MongoDB collections and indexes:
//   - telemetry:     time series (timeField=ts, metaField=meta,
//     granularity=minutes) with expireAfterSeconds=86400 (24h TTL).
//     meta = {deviceId, site, zone} — the source id plus fixed labels, so a
//     site filter can skip whole buckets (MongoDB time series best practice).
//   - devices:       device registry (unique deviceId, $jsonSchema validation)
//   - device_state:  digital twin (reported/desired; change-streamable)
//   - commands:      command lifecycle (QUEUED..ACKED, latencyMs)
//   - events:        unified audit log with 7d TTL
//   - ai_incidents:  AI decision timeline
//   - ai_approvals:  class allowlist (unique faultClass)
//   - ai_policy:     guardrail thresholds + kill switch (single doc)
//
// It also seeds the demo fleet (12 freezers) and the default policy.
//
// usage: MONGO_URI=... MONGO_DB=coldtrace go run ./tools/initdb
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// demoFleet — 12 freezers across 3 stores, with realistic metadata.
func demoFleet() []any {
	type d struct {
		id      string
		store   string
		zone    string
		product string
		varUSD  int
	}
	base := []d{
		{"FZ-001", "OM-DNG-01", "A1", "premium beef", 8000},
		{"FZ-002", "OM-DNG-01", "A1", "premium beef", 8000},
		{"FZ-003", "OM-DNG-01", "A2", "seafood", 12000},
		{"FZ-004", "OM-DNG-01", "A2", "seafood", 12000},
		{"FZ-005", "OM-DNG-01", "B1", "ice cream", 6000},
		{"FZ-006", "OM-DNG-02", "A1", "premium beef", 8000},
		{"FZ-007", "OM-DNG-02", "A1", "premium beef", 8000},
		{"FZ-008", "OM-DNG-02", "B1", "ice cream", 6000},
		{"FZ-009", "OM-DNG-03", "A1", "seafood", 12000},
		{"FZ-010", "OM-DNG-03", "A2", "vaccines", 25000},
		{"FZ-011", "OM-DNG-03", "B2", "prepared meals", 4000},
		{"FZ-012", "OM-DNG-03", "B2", "prepared meals", 4000},
	}
	out := make([]any, 0, len(base))
	for _, x := range base {
		out = append(out, bson.M{
			"deviceId":       x.id,
			"site":           x.store,
			"zone":           x.zone,
			"product":        x.product,
			"valueAtRisk":    x.varUSD,
			"setpoint":       -18.0,
			"alarmAboveC":    -10.0,
			"atRiskAboveC":   -14.0,
			"model":          "ColdAire CA-400",
			"commissionedAt": time.Date(2023, 3, 14, 0, 0, 0, 0, time.UTC),
		})
	}
	return out
}

// deviceValidator — $jsonSchema for the devices registry.
func deviceValidator() options.Lister[options.CreateCollectionOptions] {
	return options.CreateCollection().SetValidator(bson.M{
		"$jsonSchema": bson.M{
			"bsonType": "object",
			"required": []string{"deviceId", "site", "zone", "product", "valueAtRisk", "setpoint"},
			"properties": bson.M{
				"deviceId":    bson.M{"bsonType": "string", "pattern": "^[A-Z]{2}-\\d{3}$"},
				"site":        bson.M{"bsonType": "string"},
				"zone":        bson.M{"bsonType": "string"},
				"product":     bson.M{"bsonType": "string"},
				"valueAtRisk": bson.M{"bsonType": "int", "minimum": 0},
				"setpoint":    bson.M{"bsonType": "double", "maximum": 0, "minimum": -30},
			},
		},
	})
}

func main() {
	uri := os.Getenv("MONGO_URI")
	dbName := os.Getenv("MONGO_DB")
	if dbName == "" {
		dbName = "coldtrace"
	}
	if uri == "" {
		fmt.Println("MONGO_URI is required")
		os.Exit(1)
	}

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		log.Fatalf("mongo.Connect: %v", err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Ping(ctx, nil); err != nil {
		log.Fatalf("ping failed: %v", err)
	}
	db := client.Database(dbName)
	log.Printf("ping OK, db=%s", dbName)

	create := func(name string, opts ...options.Lister[options.CreateCollectionOptions]) error {
		err := db.CreateCollection(ctx, name, opts...)
		if err != nil {
			// idempotent re-runs: collection already there (or legacy plain
			// collection instead of a time-series one) — keep going
			if strings.Contains(err.Error(), "NamespaceExists") ||
				strings.Contains(err.Error(), "already exists") {
				fmt.Printf("exists (kept): %s\n", name)
				return nil
			}
		}
		return err
	}

	// legacy guard: a PLAIN telemetry collection (created by an old initdb or
	// a hand-written script) silently misses the 24h TTL and the window
	// functions that rely on the time-series storage — surface it loudly
	// instead of printing "exists (kept)" and moving on.
	if cur, lerr := db.ListCollections(ctx, bson.M{"name": "telemetry"}); lerr == nil {
		for cur.Next(ctx) {
			var info struct {
				Name    string `bson:"name"`
				Type    string `bson:"type"`
				Options struct {
					ExpireAfterSeconds *int64 `bson:"expireAfterSeconds"`
					TimeSeries         *struct {
						MetaField string `bson:"metaField"`
					} `bson:"timeseries"`
				} `bson:"options"`
			}
			if derr := cur.Decode(&info); derr != nil {
				continue
			}
			if info.Type != "timeseries" {
				log.Fatalf("telemetry exists as a PLAIN collection (type=%q): TTL is not enforced and window scans grow unbounded. "+
					"Drop it and re-run (docker compose exec mongodb mongosh coldtrace --eval 'db.telemetry.drop()') — data in it is re-ingestible by the simulator.", info.Type)
			}
			// metaField is immutable after creation — a stale schema silently
			// breaks every meta.* filter, so fail loudly instead of limping.
			if info.Options.TimeSeries == nil || info.Options.TimeSeries.MetaField != "meta" {
				got := "<none>"
				if info.Options.TimeSeries != nil {
					got = info.Options.TimeSeries.MetaField
				}
				log.Fatalf("telemetry metaField is %q, expected \"meta\" (immutable — must recreate). "+
					"Run: docker compose exec mongodb mongosh coldtrace --eval 'db.telemetry.drop()' then re-run this tool", got)
			}
			if info.Options.ExpireAfterSeconds == nil || *info.Options.ExpireAfterSeconds != int64(24*60*60) {
				log.Printf("WARNING: telemetry is a time series but expireAfterSeconds is not 86400 — old data will pile up (collMod to fix)")
			}
		}
	}

	// 1) Telemetry time series collection with 24h TTL
	if err := create("telemetry",
		options.CreateCollection().
			SetTimeSeriesOptions(options.TimeSeries().
				SetTimeField("ts").
				SetMetaField("meta").
				SetGranularity("minutes")).
			SetExpireAfterSeconds(int64(24*60*60)),
	); err != nil {
		log.Fatalf("telemetry: %v", err)
	}
	fmt.Println("created: telemetry (time series, expireAfterSeconds=86400)")

	// 2) Devices registry (unique deviceId, schema-validated)
	if err := create("devices", deviceValidator()); err != nil {
		log.Fatalf("devices: %v", err)
	}
	if _, err := db.Collection("devices").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "deviceId", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		log.Fatalf("devices index: %v", err)
	}
	n, _ := db.Collection("devices").CountDocuments(ctx, bson.D{})
	if n == 0 {
		if _, err := db.Collection("devices").InsertMany(ctx, demoFleet()); err != nil {
			log.Fatalf("devices seed: %v", err)
		}
	}
	fmt.Println("created+seeded: devices (12 freezers, $jsonSchema validated)")

	// 3) device_state twin (plain collection — change-streamable)
	if err := create("device_state"); err != nil {
		log.Fatalf("device_state: %v", err)
	}
	if _, err := db.Collection("device_state").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "deviceId", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		log.Fatalf("device_state index: %v", err)
	}
	fmt.Println("created: device_state (twin, unique deviceId)")

	// 4) commands lifecycle
	if err := create("commands"); err != nil {
		log.Fatalf("commands: %v", err)
	}
	_, err = db.Collection("commands").Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "commandId", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "deviceId", Value: 1}, {Key: "createdAt", Value: -1}}},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "expiresAt", Value: 1}}},
	})
	if err != nil {
		log.Fatalf("commands indexes: %v", err)
	}
	fmt.Println("created: commands (unique commandId, lifecycle indexes)")

	// 5) events — unified audit log with 7d TTL
	if err := create("events"); err != nil {
		log.Fatalf("events: %v", err)
	}
	if _, err := db.Collection("events").Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "at", Value: -1}}},
		{Keys: bson.D{{Key: "type", Value: 1}, {Key: "at", Value: -1}}},
		{Keys: bson.D{{Key: "deviceId", Value: 1}, {Key: "at", Value: -1}}},
		{Keys: bson.D{{Key: "expiresAt", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(int32(0))},
	}); err != nil {
		log.Fatalf("events indexes: %v", err)
	}
	fmt.Println("created: events (audit log, TTL 7d via expiresAt)")

	// 6) incidents (legacy gateway timeline) + ai_incidents
	if err := create("incidents"); err != nil {
		log.Fatalf("incidents: %v", err)
	}
	if _, err := db.Collection("incidents").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "deviceId", Value: 1}, {Key: "openedAt", Value: -1}},
	}); err != nil {
		log.Fatalf("incidents index: %v", err)
	}
	if err := create("ai_incidents"); err != nil {
		log.Fatalf("ai_incidents: %v", err)
	}
	if _, err := db.Collection("ai_incidents").Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "openedAt", Value: -1}}},
		{Keys: bson.D{{Key: "deviceId", Value: 1}, {Key: "faultClass", Value: 1}, {Key: "status", Value: 1}}},
	}); err != nil {
		log.Fatalf("ai_incidents indexes: %v", err)
	}
	fmt.Println("created: incidents + ai_incidents (deviceId+openedAt, status indexes)")

	// 7) approvals (unique class) + policy defaults
	if err := create("ai_approvals"); err != nil {
		log.Fatalf("ai_approvals: %v", err)
	}
	if _, err := db.Collection("ai_approvals").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "faultClass", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		log.Fatalf("ai_approvals index: %v", err)
	}
	if err := create("ai_policy"); err != nil {
		log.Fatalf("ai_policy: %v", err)
	}
	nPolicy, _ := db.Collection("ai_policy").CountDocuments(ctx, bson.D{})
	if nPolicy == 0 {
		_, _ = db.Collection("ai_policy").InsertOne(ctx, bson.M{
			"minConfidence":       0.85,
			"maxEtaAlarmMinutes":  10.0,
			"autoCooldownMinutes": 30,
			"fleetCircuitBreaker": 2,
			"killSwitch":          false,
			"updatedAt":           time.Now().UTC(),
		})
	}
	fmt.Println("created: ai_approvals (unique class) + ai_policy (guardrails + kill switch)")

	// 8) alert_rules (operator-authored thresholds evaluated by ops-api)
	if err := create("alert_rules"); err != nil {
		log.Fatalf("alert_rules: %v", err)
	}
	if _, err := db.Collection("alert_rules").Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "ruleId", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "deviceId", Value: 1}, {Key: "createdAt", Value: -1}}},
	}); err != nil {
		log.Fatalf("alert_rules indexes: %v", err)
	}
	fmt.Println("created: alert_rules (unique ruleId, device+createdAt)")

	// Rollup summary collections (warm tier). $merge requires a UNIQUE index
	// on exactly its `on` fields — creating them here means the rollup
	// service works on a fresh database without a manual step.
	for _, tgt := range []string{"telemetry_hourly", "telemetry_daily"} {
		if err := create(tgt); err != nil {
			log.Fatalf("%s: %v", tgt, err)
		}
		if _, err := db.Collection(tgt).Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "deviceId", Value: 1}, {Key: "bucket", Value: 1}},
			Options: options.Index().SetName("uniq_deviceId_bucket").SetUnique(true),
		}); err != nil {
			log.Writer().Write([]byte("WARN: " + tgt + " unique index (may already exist): " + err.Error() + "\n"))
		}
	}
	fmt.Println("created: telemetry_hourly + telemetry_daily (unique deviceId+bucket for $merge)")

	// Query index from the workshop's performance tips: filter on metaField
	// then time. Time series collections accept non-unique indexes.
	if _, err := db.Collection("telemetry").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "meta.deviceId", Value: 1}, {Key: "ts", Value: -1}},
		Options: options.Index().SetName("meta.deviceId_1_ts_-1"),
	}); err != nil {
		log.Writer().Write([]byte("WARN: telemetry index (may already exist): " + err.Error() + "\n"))
	}

	fmt.Println("all MongoDB collections/indexes ready")
}
