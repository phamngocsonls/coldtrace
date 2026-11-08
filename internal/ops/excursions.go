// Package ops — excursions.go builds the HACCP-style excursion audit.
//
// This pipeline lived inline in cmd/ops-api/main.go until the audit called
// it out: every date operator ran in UTC and nothing was formatted, so the
// CSV an inspector downloads showed UTC instants — 7 hours off the wall
// clock a Vietnamese food-safety auditor works with. It lives here now,
// with the tz-aware "last excursion seen" column (via $dateToString) and
// tests pinning its shape (see excursions_test.go).
//
// Minutes derive from actual ts deltas ($setWindowFields/$shift over the
// excursion spans), not a hardcoded cadence — the loadgen profile runs at
// 1s and a device that drops offline mid-excursion must not inflate the
// audit. A gap (device offline, first/last sample) stops the clock: only
// consecutive samples <= 10s apart are credited.
package ops

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// ExcursionsPipeline aggregates per-device minutes above the -14C at-risk
// threshold over the window, with a tz-formatted local timestamp of the
// last excursion sample.
//
// Read-only by construction (no $merge/$out).
func ExcursionsPipeline(since time.Time, tz string) bson.A {
	return bson.A{
		bson.D{{Key: "$match", Value: bson.M{
			"ts":    bson.M{"$gte": since},
			"tempC": bson.M{"$gt": -14},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "ts", Value: 1}}}},
		bson.D{{Key: "$setWindowFields", Value: bson.D{
			{Key: "partitionBy", Value: "$meta.deviceId"},
			{Key: "sortBy", Value: bson.D{{Key: "ts", Value: 1}}},
			{Key: "output", Value: bson.D{
				{Key: "prevTs", Value: bson.D{{Key: "$shift", Value: bson.D{
					{Key: "output", Value: "$ts"},
					{Key: "by", Value: -1},
				}}}},
			}},
		}}},
		bson.D{{Key: "$addFields", Value: bson.D{
			// span covered by this excursion sample: up to 10s credited —
			// a gap (device offline, first/last sample) stops the clock
			{Key: "spanMs", Value: bson.D{{Key: "$cond", Value: bson.A{
				bson.D{{Key: "$and", Value: bson.A{
					bson.D{{Key: "$ne", Value: bson.A{"$prevTs", nil}}},
					bson.D{{Key: "$lte", Value: bson.A{
						bson.D{{Key: "$subtract", Value: bson.A{"$ts", "$prevTs"}}},
						10000,
					}}},
				}}},
				bson.D{{Key: "$subtract", Value: bson.A{"$ts", "$prevTs"}}},
				0,
			}}}},
		}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$meta.deviceId"},
			{Key: "excursionMs", Value: bson.D{{Key: "$sum", Value: "$spanMs"}}},
			{Key: "worstTempC", Value: bson.D{{Key: "$max", Value: "$tempC"}}},
			{Key: "samples", Value: bson.D{{Key: "$sum", Value: 1}}},
			{Key: "lastExcursionAt", Value: bson.D{{Key: "$max", Value: "$ts"}}},
		}}},
		bson.D{{Key: "$addFields", Value: bson.D{
			{Key: "excursionMin", Value: bson.D{{Key: "$divide", Value: bson.A{"$excursionMs", 60000}}}},
			// Inspector-facing: the LAST excursion instant rendered in the
			// audit timezone — the explicit-tz rule from the brief, applied
			// to the read path too.
			{Key: "lastExcursionLocal", Value: bson.D{{Key: "$dateToString", Value: bson.D{
				{Key: "date", Value: "$lastExcursionAt"},
				{Key: "format", Value: "%Y-%m-%d %H:%M"},
				{Key: "timezone", Value: tz},
			}}}},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "excursionMin", Value: -1}}}},
	}
}

// ExcursionRow is one aggregated audit line.
type ExcursionRow struct {
	ID                 string  `bson:"_id"`
	ExcursionMin       float64 `bson:"excursionMin"`
	WorstTempC         float64 `bson:"worstTempC"`
	Samples            int64   `bson:"samples"`
	LastExcursionLocal string  `bson:"lastExcursionLocal"`
}
