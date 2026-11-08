// Command seedhistory warms a fresh ColdTrace stack with demo HISTORY.
//
// Two cold-start gaps, both found by the pre-demo audit:
//
//  1. The AI incident memory (ai.IncidentMemory) scans RESOLVED
//     incidents — a fresh stack has none, so the "similar past incidents"
//     panel renders empty on stage. Seeded incidents come from the same
//     simulator physics the agent classifies at serve time, so the memory
//     is exercised on its real distribution.
//
//  2. The warm/cold tiers only accumulate from stack start-up, so the
//     tier chart (GET /api/devices/{id}/history) shows a few hours.
//     Seeded history makes the retention story visible: days of hourly,
//     weeks of daily — strictly OLDER than the rollup's live window, so
//     nothing collides with its $merge unique index.
//
// Idempotent: everything this tool writes carries seeded:true and a
// re-run replaces exactly that set (never live agent incidents, never
// rollup-written buckets).
//
// usage:
//
//	MONGO_URI=mongodb://mongodb:27017 go run ./tools/seedhistory
//	go run ./tools/seedhistory -incidents 40 -hourly-days 5 -daily-days 30
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/onemount/coldtrace/internal/seed"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// deleteRange removes exactly the window the generated docs cover — fleet
// devices, oldest generated bucket inclusive to newest exclusive — so the
// insert cannot collide with the $merge unique index. Live rollup buckets
// (its window is only the last 3h) are outside this range by construction.
func deleteRange(ctx context.Context, coll *mongo.Collection, docs []any) (int64, error) {
	if len(docs) == 0 {
		return 0, nil
	}
	lo, hi := time.Time{}, time.Time{}
	fleet := bson.A{}
	for _, d := range docs {
		s, ok := d.(seed.Summary)
		if !ok {
			continue
		}
		fleet = appendOnce(fleet, s.DeviceID)
		if lo.IsZero() || s.Bucket.Before(lo) {
			lo = s.Bucket
		}
		if hi.IsZero() || s.Bucket.After(hi) {
			hi = s.Bucket
		}
	}
	res, err := coll.DeleteMany(ctx, bson.M{
		"deviceId": bson.M{"$in": fleet},
		"bucket":   bson.M{"$gte": lo, "$lte": hi},
	})
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

// appendOnce adds v to the list if not already present (deviceId list).
func appendOnce(list bson.A, v any) bson.A {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func main() {
	incidents := flag.Int("incidents", 24, "resolved historical incidents to seed")
	hourlyDays := flag.Int("hourly-days", 6, "days of warm-tier (telemetry_hourly) history; 0 disables")
	dailyDays := flag.Int("daily-days", 21, "days of cold-tier (telemetry_daily) history; 0 disables")
	flag.Parse()

	ctx, contextCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer contextCancel()

	client, err := mongo.Connect(options.Client().
		ApplyURI(env("MONGO_URI", "mongodb://mongodb:27017")).
		SetAppName("coldtrace-seedhistory"))
	if err != nil {
		log.Fatalf("mongo connect: %v", err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	if err := client.Ping(ctx, nil); err != nil {
		log.Fatalf("mongo ping: %v", err)
	}
	db := client.Database(env("MONGO_DB", "coldtrace"))
	now := time.Now().UTC()

	// 1) Historical incidents for the AI incident memory.
	if *incidents > 0 {
		inc := db.Collection("ai_incidents")
		del, err := inc.DeleteMany(ctx, seed.ReplaceFilter())
		if err != nil {
			log.Fatalf("incidents delete: %v", err)
		}
		docs := seed.IncidentDocs(seed.Incidents(*incidents, now))
		if len(docs) > 0 {
			// { ordered: false }: one malformed doc must not reject the set.
			if _, err := inc.InsertMany(ctx, docs, options.InsertMany().SetOrdered(false)); err != nil {
				log.Fatalf("incidents insert: %v", err)
			}
		}
		fmt.Printf("seeded: %d resolved incidents (replaced %d prior seeded) -> ai_incidents\n", len(docs), del.DeletedCount)
	}

	// 2) Warm tier: hourly summaries, strictly before the current hour.
	// The tool OWNS the range it generates: real buckets the rollup wrote
	// during stack uptime overlap the seeded range, and the unique
	// deviceId+bucket index rejects them. Range-delete first (fleet
	// devices, generated window), then insert — the rollup only ever
	// recomputes its live 3h, so it never fights back over these.
	if *hourlyDays > 0 {
		coll := db.Collection("telemetry_hourly")
		docs := seed.SummaryDocs(seed.HourlyHistory(seed.FleetDevices, *hourlyDays, now, int64(*hourlyDays)))
		del, err := deleteRange(ctx, coll, docs)
		if err != nil {
			log.Fatalf("hourly delete: %v", err)
		}
		if _, err := coll.InsertMany(ctx, docs, options.InsertMany().SetOrdered(false)); err != nil {
			log.Fatalf("hourly insert: %v", err)
		}
		fmt.Printf("seeded: %d hourly buckets over %d day(s) (replaced %d) -> telemetry_hourly\n",
			len(docs), *hourlyDays, del)
	}

	// 3) Cold tier: daily summaries, strictly before local midnight today.
	if *dailyDays > 0 {
		coll := db.Collection("telemetry_daily")
		docs := seed.SummaryDocs(seed.DailyHistory(seed.FleetDevices, *dailyDays, now, int64(*dailyDays)))
		del, err := deleteRange(ctx, coll, docs)
		if err != nil {
			log.Fatalf("daily delete: %v", err)
		}
		if _, err := coll.InsertMany(ctx, docs, options.InsertMany().SetOrdered(false)); err != nil {
			log.Fatalf("daily insert: %v", err)
		}
		fmt.Printf("seeded: %d daily buckets over %d day(s) (replaced %d) -> telemetry_daily\n",
			len(docs), *dailyDays, del)
	}

	// The story the seed enables, stated where the operator will see it:
	fmt.Println()
	fmt.Println("verify:")
	fmt.Println("  curl localhost:8092/api/devices/FZ-001/history?tier=hourly   # warm tier chart")
	fmt.Println("  curl localhost:8092/api/incidents                          # resolved history")
	fmt.Println("  curl localhost:8092/api/compliance/excursions.csv          # HACCP audit (local time)")
}
