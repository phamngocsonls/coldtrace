// Package aggdb — history.go is the READ side of the hot/warm/cold tiers.
//
// The rollup (cmd/rollup) $merge's hourly and daily summaries into
// telemetry_hourly / telemetry_daily, and until this file existed nothing
// ever queried them: the tiering story was a write-only claim. This reader
// closes the loop so the console (and the demo) can show the actual
// downsampled history — hours after the raw readings TTL'd away, a year for
// the daily tier.
//
// Pipeline rules (same as everywhere in this codebase):
//   - $match FIRST on (deviceId, bucket) so MongoDB skips whole buckets
//     before anything else runs.
//   - The local bucket label is rendered server-side with $dateToString
//     and an explicit timezone — the brief's "explicit tz on date
//     operators" rule applies to reads as much as to the rollup's
//     $dateTrunc. An inspector reading "00:00" should see wall time, not a
//     UTC instant 7 hours off.
package aggdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Tier is one rollup summary collection.
type Tier string

const (
	// TierHourly is the warm tier: 1-hour buckets, 7-day retention.
	TierHourly Tier = "hourly"
	// TierDaily is the cold tier: 1-day buckets, 365-day retention.
	TierDaily Tier = "daily"
)

// VietnamTimezone matches the rollup's $dateTrunc bucketing. Labels must
// render in the same zone the buckets were cut in, or the boundaries lie.
const VietnamTimezone = "Asia/Ho_Chi_Minh"

// Collection resolves the tier to its summary collection.
func (t Tier) Collection() (string, error) {
	switch t {
	case TierHourly:
		return "telemetry_hourly", nil
	case TierDaily:
		return "telemetry_daily", nil
	default:
		return "", fmt.Errorf("unknown tier %q (want %q or %q)", string(t), TierHourly, TierDaily)
	}
}

// Unit is the bucket unit the tier was $dateTrunc'd into.
func (t Tier) Unit() string {
	if t == TierDaily {
		return "day"
	}
	return "hour"
}

// MaxLookback is the tier's retention — the honest cap on how far back a
// client may ask. Beyond it the chart is not "no data", it is "expired by
// design", and returning empty for a silently-wrong range would mislead.
func (t Tier) MaxLookback() time.Duration {
	if t == TierDaily {
		return 365 * 24 * time.Hour
	}
	return 7 * 24 * time.Hour
}

// TierPoint is one summary bucket off a tier collection.
type TierPoint struct {
	DeviceID       string    `bson:"deviceId"       json:"deviceId"`
	Bucket         time.Time `bson:"bucket"         json:"bucket"`
	BucketLocal    string    `bson:"bucketLocal"    json:"bucketLocal"`
	Unit           string    `bson:"unit"           json:"unit"`
	AvgTempC       float64   `bson:"avgTempC"        json:"avgTempC"`
	MinTempC       float64   `bson:"minTempC"       json:"minTempC"`
	MaxTempC       float64   `bson:"maxTempC"       json:"maxTempC"`
	AvgPowerDraw   float64   `bson:"avgPowerDraw"    json:"avgPowerDraw"`
	CompressorDuty float64   `bson:"compressorDuty" json:"compressorDuty"`
	DoorEvents     int64     `bson:"doorEvents"     json:"doorEvents"`
	Readings       int64     `bson:"readings"        json:"readings"`
}

// tierHistoryPipeline is the read pipeline for one device's tier history.
// Read-only by construction: no $merge/$out/$setWindowFields.
func tierHistoryPipeline(deviceID string, from time.Time) mongo.Pipeline {
	return mongo.Pipeline{
		// 1) $match FIRST — the brief's rule; bucket range lets MongoDB skip
		//    whole summary documents before any projection runs.
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "deviceId", Value: deviceID},
			{Key: "bucket", Value: bson.D{{Key: "$gte", Value: from}}},
		}}},
		// 2) oldest -> newest so the chart x-axis never needs a client sort.
		bson.D{{Key: "$sort", Value: bson.D{{Key: "bucket", Value: 1}}}},
		// 3) Project with the LOCAL label. $dateToString with an explicit
		//    timezone is the same rule the rollup applies on write — a
		//    07:00Z bucket is "14:00" on a Vietnamese wall clock, and the
		//    operator reads wall time.
		bson.D{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 0},
			{Key: "deviceId", Value: 1},
			{Key: "bucket", Value: 1},
			{Key: "bucketLocal", Value: bson.D{{Key: "$dateToString", Value: bson.D{
				{Key: "date", Value: "$bucket"},
				{Key: "format", Value: "%Y-%m-%d %H:%M"},
				{Key: "timezone", Value: VietnamTimezone},
			}}}},
			{Key: "unit", Value: 1},
			{Key: "avgTempC", Value: 1},
			{Key: "minTempC", Value: 1},
			{Key: "maxTempC", Value: 1},
			{Key: "avgPowerDraw", Value: 1},
			{Key: "compressorDuty", Value: 1},
			{Key: "doorEvents", Value: 1},
			{Key: "readings", Value: 1},
		}}},
	}
}

// TierHistory reads one device's summary history from a tier collection.
// The caller resolves the tier (ops-api validates it) and passes the
// tier's collection.
func TierHistory(ctx context.Context, coll *mongo.Collection, tier Tier, deviceID string, from time.Time) ([]TierPoint, error) {
	if _, err := tier.Collection(); err != nil {
		return nil, err
	}
	if deviceID == "" {
		return nil, errors.New("tier history requires a deviceId")
	}
	cur, err := coll.Aggregate(ctx, tierHistoryPipeline(deviceID, from))
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close(ctx) }()
	var out []TierPoint
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
