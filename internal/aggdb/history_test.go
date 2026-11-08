package aggdb

import (
	"context"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The warm/cold tiers (telemetry_hourly, telemetry_daily) were write-only
// until this reader existed: the rollup $merge'd summaries in and nothing
// ever queried them, so the hot->warm->cold story was a claim, not a
// feature. These tests pin the read path.

// TestTierHistoryPipelineShape pins the pipeline the tier reader runs:
// $match FIRST on (deviceId, bucket) — the same performance rule the write
// side follows — then a tz-aware $dateToString so the console shows the
// LOCAL bucket label, never a UTC string.
func TestTierHistoryPipelineShape(t *testing.T) {
	from := time.Now().UTC().Add(-72 * time.Hour)
	p := tierHistoryPipeline("FZ-001", from)

	if len(p) == 0 {
		t.Fatal("tierHistoryPipeline returned no stages")
	}
	if got := p[0][0].Key; got != "$match" {
		t.Fatalf("first stage is %s, want $match (time+deviceId narrow the scan)", got)
	}
	matchDoc, ok := p[0][0].Value.(bson.D)
	if !ok {
		t.Fatalf("$match value is %T, want bson.D", p[0][0].Value)
	}
	if !hasKey(matchDoc, "deviceId") || !hasKey(matchDoc, "bucket") {
		t.Error("$match must filter on both deviceId and bucket")
	}

	src := sprint(p)
	if !containsStr(src, "$dateToString") {
		t.Error("pipeline has no $dateToString — the local bucket label is the point of the reader")
	}
	if !containsStr(src, `Asia/Ho_Chi_Minh`) {
		t.Error("$dateToString must carry the Asia/Ho_Chi_Minh timezone (brief: explicit tz on date operators)")
	}

	// READ ONLY: a $merge/$out here would corrupt the summary tiers.
	for _, st := range p {
		switch st[0].Key {
		case "$merge", "$out", "$mergeObjects", "$setWindowFields":
			t.Errorf("read pipeline must not write or window: found %s", st[0].Key)
		}
	}
}

func hasKey(doc bson.D, key string) bool {
	for _, f := range doc {
		if f.Key == key {
			return true
		}
	}
	return false
}

// TestTierResolution pins the tier->collection mapping and the retention
// bound that caps how far back a client may ask (asking beyond retention
// would return a misleading empty chart).
func TestTierResolution(t *testing.T) {
	cases := []struct {
		tier   Tier
		coll   string
		unit   string
		maxAge time.Duration
	}{
		{TierHourly, "telemetry_hourly", "hour", 7 * 24 * time.Hour},
		{TierDaily, "telemetry_daily", "day", 365 * 24 * time.Hour},
	}
	for _, c := range cases {
		coll, err := c.tier.Collection()
		if err != nil {
			t.Fatalf("%v: %v", c.tier, err)
		}
		if coll != c.coll {
			t.Errorf("%v.Collection() = %s, want %s", c.tier, coll, c.coll)
		}
		if u := c.tier.Unit(); u != c.unit {
			t.Errorf("%v.Unit() = %s, want %s", c.tier, u, c.unit)
		}
		if m := c.tier.MaxLookback(); m != c.maxAge {
			t.Errorf("%v.MaxLookback() = %v, want %v (rollup retention)", c.tier, m, c.maxAge)
		}
	}
	if _, err := Tier("weekly").Collection(); err == nil {
		t.Error("unknown tier must be rejected, not silently resolve")
	}
}

// testMongo connects to the test MongoDB (MONGO_URI or localhost) and
// returns nil, nil when unreachable so callers can t.Skip. The stack's own
// integration tests follow the same skip-don't-fail convention.
func testMongo(t *testing.T) (*mongo.Client, *mongo.Database) {
	t.Helper()
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		uri = "mongodb://localhost:27017"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetServerSelectionTimeout(2 * time.Second))
	if err != nil {
		return nil, nil
	}
	if err := client.Ping(ctx, nil); err != nil {
		return nil, nil
	}
	return client, client.Database("coldtrace")
}

// TestTierHistoryRoundTrip is an integration test against a real MongoDB
// (skipped when unreachable): seed three hourly buckets, read them back
// through TierHistory, and assert the round trip — including the local
// label, which must render as Asia/Ho_Chi_Minh wall time.
func TestTierHistoryRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mclient, db := testMongo(t)
	if mclient == nil {
		t.Skip("no mongodb reachable — run with the compose stack up")
	}
	defer func() { _ = mclient.Disconnect(ctx) }()

	coll := db.Collection("telemetry_hourly")
	// unique index is (deviceId, bucket); a dedicated device keeps the
	// fixture out of the rollup's real data.
	const dev = "TEST-TIER-HIST"
	_, _ = coll.DeleteMany(ctx, bson.M{"deviceId": dev})
	buckets := []time.Time{
		mustLocalHour(t, 26),
		mustLocalHour(t, 25),
		mustLocalHour(t, 24),
	}
	docs := make([]any, len(buckets))
	for i, b := range buckets {
		docs[i] = bson.M{
			"deviceId": dev, "bucket": b, "unit": "hour",
			"avgTempC": -18.5, "minTempC": -19.2, "maxTempC": -17.9,
			"avgPowerDraw": 412.5, "compressorDuty": 0.44,
			"doorEvents": int64(2), "readings": int64(720),
		}
	}
	if _, err := coll.InsertMany(ctx, docs, options.InsertMany().SetOrdered(false)); err != nil {
		t.Fatalf("fixture insert: %v", err)
	}
	defer func() { _, _ = coll.DeleteMany(ctx, bson.M{"deviceId": dev}) }()

	pts, err := TierHistory(ctx, coll, TierHourly, dev, mustLocalHour(t, 48))
	if err != nil {
		t.Fatalf("TierHistory: %v", err)
	}
	if len(pts) != 3 {
		t.Fatalf("got %d points, want 3 (seeded 26/25/24h ago)", len(pts))
	}
	if !pts[0].Bucket.Before(pts[1].Bucket) || !pts[1].Bucket.Before(pts[2].Bucket) {
		t.Error("points are not sorted ascending by bucket")
	}
	if pts[0].BucketLocal == "" {
		t.Error("bucketLocal is empty — the tz-aware label is missing")
	}
	// The invariant that matters: bucketLocal is the bucket rendered on a
	// Vietnamese wall clock (UTC+7), not the UTC instant.
	loc := time.FixedZone("ICT", 7*60*60)
	if want := pts[0].Bucket.In(loc).Format("2006-01-02 15:04"); pts[0].BucketLocal != want {
		t.Errorf("bucketLocal = %q, want %q (bucket + 7h, Asia/Ho_Chi_Minh wall time)", pts[0].BucketLocal, want)
	}
	if pts[0].AvgTempC != -18.5 || pts[0].Readings != 720 {
		t.Errorf("round trip mismatch: %+v", pts[0])
	}
}

// mustLocalHour returns a UTC instant exactly `hoursAgo` whole local hours
// back, aligned to the local hour so it collides with nothing the rollup
// writes (rollup only recomputes the last 3h).
func mustLocalHour(t *testing.T, hoursAgo int) time.Time {
	t.Helper()
	loc := time.FixedZone("ICT", 7*60*60)
	now := time.Now().In(loc)
	h := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, loc)
	return h.Add(-time.Duration(hoursAgo) * time.Hour).UTC()
}
