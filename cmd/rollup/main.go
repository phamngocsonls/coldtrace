// Command rollup downsamples the telemetry time series collection into
// hourly and daily summaries — the "warm" tier of the hot/warm/cold model
// from the MongoDB Time Series workshop (slides 22 and 24).
//
// Raw readings live in the time series collection with a 24 h TTL. The hourly
// and daily summaries are PLAIN collections with a much longer retention, so
// trends stay queryable long after the raw points expire.
//
// Two rules from the brief are load-bearing here:
//
//  1. $merge writes into a normal collection. ($out is the time series
//     variant.) We use $merge so re-running a window is idempotent —
//     whenMatched:"merge" updates the bucket, and no duplicate summary rows
//     accumulate if the loop overlaps.
//  2. Dates are stored in UTC, so every $dateTrunc passes an explicit
//     timezone. Without it a "day" would end at 07:00 local time in Vietnam.
//
// usage:
//
//	MONGO_URI=mongodb://mongodb:27017 MONGO_DB=coldtrace go run ./cmd/rollup
//	go run ./cmd/rollup -once        # single pass, then exit (CI/demo)
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/onemount/coldtrace/internal/otlp"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Rollup is one downsample target.
type Rollup struct {
	// Name is used in logs and the span name.
	Name string
	// Source is the raw time series collection.
	Source string
	// Target is the summary collection (plain).
	Target string
	// Unit is the $dateTrunc unit: "hour" or "day".
	Unit string
	// Lookback bounds how far back each pass re-computes. Re-computing a
	// small window is what makes the job self-healing after downtime.
	Lookback time.Duration
	// Retention drops summary documents older than this (0 = keep forever).
	Retention time.Duration
}

// DefaultRollups is the hot/warm split described on slide 22: keep raw
// readings for 24h (TTL on the source collection), hourly summaries for a
// week, daily summaries for a year.
func DefaultRollups() []Rollup {
	return []Rollup{
		{Name: "hourly", Source: "telemetry", Target: "telemetry_hourly", Unit: "hour",
			Lookback: 3 * time.Hour, Retention: 7 * 24 * time.Hour},
		{Name: "daily", Source: "telemetry", Target: "telemetry_daily", Unit: "day",
			Lookback: 48 * time.Hour, Retention: 365 * 24 * time.Hour},
	}
}

func main() {
	once := flag.Bool("once", false, "run one rollup pass and exit")
	interval := flag.Duration("interval", 5*time.Minute, "delay between rollup passes")
	tz := flag.String("timezone", env("ROLLUP_TZ", "Asia/Ho_Chi_Minh"),
		`timezone for $dateTrunc bucketing (the brief requires an explicit tz)`)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "coldtrace-rollup")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopTrace, err := otlp.StartTrace(ctx, "coldtrace-rollup")
	if err != nil {
		logger.Warn("otlp trace init", "err", err)
	}
	stopMetric, _ := otlp.StartMetric(ctx, "coldtrace-rollup")
	_, logProvider, _ := otlp.StartLogs(ctx, "coldtrace-rollup")
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

	// -1 lifts the driver's default 1h pool idle timeout so a long-lived
	// service does not churn connections between passes.
	mclient, err := mongo.Connect(options.Client().
		ApplyURI(env("MONGO_URI", "mongodb://mongodb:27017")).
		SetAppName("coldtrace-rollup"))
	if err != nil {
		logger.Error("mongo connect", "err", err)
		os.Exit(1)
	}
	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pingCancel()
	if err := mclient.Ping(pingCtx, nil); err != nil {
		logger.Error("mongo ping failed", "err", err)
		os.Exit(1)
	}
	db := mclient.Database(env("MONGO_DB", "coldtrace"))

	tracer := otel.Tracer("coldtrace.rollup")
	run := func() {
		now := time.Now().UTC()
		for _, r := range DefaultRollups() {
			if err := runRollup(ctx, tracer, logger, db, r, *tz, now); err != nil {
				logger.Error("rollup_failed", "rollup", r.Name, "err", err)
				continue
			}
			if r.Retention > 0 {
				if err := enforceRetention(ctx, logger, db, r); err != nil {
					logger.Warn("rollup_retention_failed", "rollup", r.Name, "err", err)
				}
			}
		}
	}

	logger.Info("rollup online", "timezone", *tz, "interval", interval.String(), "once", *once)
	run()
	if *once {
		return
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			run()
		case <-sig:
			logger.Info("rollup_shutdown")
			return
		}
	}
}

// runRollup executes one $group -> $merge downsampling pass.
func runRollup(ctx context.Context, tracer trace.Tracer, log *slog.Logger, db *mongo.Database, r Rollup, tz string, now time.Time) error {
	ctx, span := tracer.Start(ctx, "rollup."+r.Name,
		trace.WithAttributes(
			attribute.String("db.collection.source", r.Source),
			attribute.String("db.collection.target", r.Target),
			attribute.String("rollup.unit", r.Unit),
			attribute.String("rollup.timezone", tz),
		))
	defer span.End()

	from := now.Add(-r.Lookback)
	pipeline := mongo.Pipeline{
		// 1) $match FIRST, on time — the brief's performance tip and what
		// lets MongoDB skip whole buckets.
		bson.D{{Key: "$match", Value: bson.D{{Key: "ts", Value: bson.D{
			{Key: "$gte", Value: from},
			{Key: "$lt", Value: now},
		}}}}},
		// 2) $group per device per truncated time bucket, with an explicit
		// timezone so "day"/"hour" mean local time, not UTC.
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{
				{Key: "deviceId", Value: "$meta.deviceId"},
				{Key: "bucket", Value: bson.D{{Key: "$dateTrunc", Value: bson.D{
					{Key: "date", Value: "$ts"},
					{Key: "unit", Value: r.Unit},
					{Key: "timezone", Value: tz},
				}}}},
			}},
			{Key: "deviceId", Value: bson.D{{Key: "$first", Value: "$meta.deviceId"}}},
			{Key: "avgTempC", Value: bson.D{{Key: "$avg", Value: "$tempC"}}},
			{Key: "minTempC", Value: bson.D{{Key: "$min", Value: "$tempC"}}},
			{Key: "maxTempC", Value: bson.D{{Key: "$max", Value: "$tempC"}}},
			{Key: "avgPowerDraw", Value: bson.D{{Key: "$avg", Value: "$powerDraw"}}},
			{Key: "compressorDuty", Value: bson.D{{Key: "$avg", Value: bson.D{{Key: "$cond", Value: bson.A{"$compressorRunning", 1, 0}}}}}},
			{Key: "doorEvents", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{"$doorOpen", 1, 0}}}}}},
			{Key: "readings", Value: bson.D{{Key: "$count", Value: bson.D{}}}},
		}}},
		// 3) $project back into a readable document shape.
		bson.D{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 0},
			{Key: "deviceId", Value: 1},
			{Key: "bucket", Value: "$_id.bucket"},
			{Key: "unit", Value: r.Unit},
			{Key: "avgTempC", Value: 1},
			{Key: "minTempC", Value: 1},
			{Key: "maxTempC", Value: 1},
			{Key: "avgPowerDraw", Value: 1},
			{Key: "compressorDuty", Value: 1},
			{Key: "doorEvents", Value: 1},
			{Key: "readings", Value: 1},
		}}},
		// 4) $merge into the summary collection. whenMatched:"merge" makes
		// the pass idempotent: re-running a window updates in place.
		bson.D{{Key: "$merge", Value: bson.D{
			{Key: "into", Value: r.Target},
			{Key: "on", Value: bson.A{"deviceId", "bucket"}},
			{Key: "whenMatched", Value: "merge"},
			{Key: "whenNotMatched", Value: "insert"},
		}}},
	}

	cur, err := db.Collection(r.Source).Aggregate(ctx, pipeline)
	if err != nil {
		return err
	}
	defer func() { _ = cur.Close(ctx) }()
	if err := cur.Err(); err != nil {
		return err
	}

	// NOTE: $merge is a terminal stage that writes and returns NO documents,
	// so the cursor is always empty. Reporting its result would log a
	// misleading "readings: 0" — count the source window and the target
	// instead, which is the number the demo actually wants to show.
	buckets := 0
	for cur.Next(ctx) {
		buckets++
	}
	if err := cur.Err(); err != nil {
		return err
	}

	raw, err := db.Collection(r.Source).CountDocuments(ctx, bson.D{{Key: "ts", Value: bson.D{
		{Key: "$gte", Value: from}, {Key: "$lt", Value: now},
	}}})
	if err != nil {
		return err
	}
	summary, err := db.Collection(r.Target).CountDocuments(ctx, bson.D{{Key: "bucket", Value: bson.D{
		{Key: "$gte", Value: from}, {Key: "$lt", Value: now},
	}}})
	if err != nil {
		return err
	}

	// The storage-savings story for the demo: N raw readings collapse to M
	// summary buckets.
	ratio := 0.0
	if summary > 0 {
		ratio = float64(raw) / float64(summary)
	}
	log.Info("rollup_pass", "rollup", r.Name, "source", r.Source,
		"target", r.Target, "rawReadings", raw, "summaryBuckets", summary,
		"compression", fmt.Sprintf("%.1fx", ratio), "timezone", tz)
	span.SetAttributes(
		attribute.Int64("rollup.raw_readings", raw),
		attribute.Int64("rollup.summary_buckets", summary),
		attribute.Float64("rollup.compression_ratio", ratio),
	)
	return nil
}

// enforceRetention trims summary documents older than the rollup's retention.
func enforceRetention(ctx context.Context, log *slog.Logger, db *mongo.Database, r Rollup) error {
	cutoff := time.Now().UTC().Add(-r.Retention)
	res, err := db.Collection(r.Target).DeleteMany(ctx, bson.D{{Key: "bucket", Value: bson.D{{Key: "$lt", Value: cutoff}}}})
	if err != nil {
		return err
	}
	if res.DeletedCount > 0 {
		log.Info("rollup_retention", "rollup", r.Name, "deleted", res.DeletedCount)
	}
	return nil
}

// EnsureIndexes creates the unique compound index $merge requires.
//
// The index MUST be unique on exactly the `on` fields: $merge refuses to run
// ("Cannot find index to verify that join fields will be unique") without it.
// Uniqueness is also what makes the pass idempotent — re-running a window
// merges into the existing bucket instead of inserting a duplicate.
func EnsureIndexes(ctx context.Context, db *mongo.Database) error {
	for _, r := range DefaultRollups() {
		_, err := db.Collection(r.Target).Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "deviceId", Value: 1}, {Key: "bucket", Value: 1}},
			Options: options.Index().SetName("uniq_deviceId_bucket").SetUnique(true),
		})
		if err != nil {
			return fmt.Errorf("%s index: %w", r.Target, err)
		}
	}
	return nil
}
