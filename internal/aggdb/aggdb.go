// Package aggdb holds MongoDB aggregation pipelines shared by the gateway
// HTTP API and the Datadog Agent custom_queries fleet-rollup (see
// datadog/agent/conf.d/mongo.d/conf.yaml).
package aggdb

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

const collTelemetry = "telemetry"

// slopeStage computes per-device °C/minute derivative of temperature
// (window function, range -5..0 minutes).
func slopeStage() bson.D {
	return bson.D{{Key: "$setWindowFields", Value: bson.D{
		{Key: "partitionBy", Value: "$meta.deviceId"},
		{Key: "sortBy", Value: bson.D{{Key: "ts", Value: 1}}},
		{Key: "output", Value: bson.D{
			// 5 minutes, matching avgTemp5m. A 2-minute derivative is shorter
			// than one thermostat cycle, so its value is dominated by where in
			// the cycle the window happens to fall: a healthy cabinet that is
			// coasting reads a rising ~+0.1 °C/min and trips the AT_RISK
			// threshold on noise alone. Over 5 minutes the cycle averages out
			// and only a genuine ramp survives.
			{Key: "slope", Value: bson.D{
				{Key: "$derivative", Value: bson.D{{Key: "input", Value: "$tempC"}, {Key: "unit", Value: "minute"}}},
				{Key: "window", Value: bson.D{
					{Key: "range", Value: bson.A{-5, 0}},
					{Key: "unit", Value: "minute"},
				}},
			}},
		}},
	}}}
}

// avgStage computes the 5-minute rolling average per device
// (window function, range -5..0 minutes).
func avgStage() bson.D {
	return bson.D{{Key: "$setWindowFields", Value: bson.D{
		{Key: "partitionBy", Value: "$meta.deviceId"},
		{Key: "sortBy", Value: bson.D{{Key: "ts", Value: 1}}},
		{Key: "output", Value: bson.D{
			{Key: "tempAvg5m", Value: bson.D{
				{Key: "$avg", Value: "$tempC"},
				{Key: "window", Value: bson.D{
					{Key: "range", Value: bson.A{-5, 0}},
					{Key: "unit", Value: "minute"},
				}},
			}},
		}},
	}}}
}

// atRiskSlopeCPM is the temperature trend at which a cold cabinet is called
// AT_RISK. It sits between two measured distributions, not at a round number.
//
// Healthy fleet, ~50k 5-minute windows from the device simulator: median
// +0.000, p99 +0.054, p99.9 +0.090, max +0.105 °C/min. A thermostat cycles,
// so a resting cabinet genuinely produces short rising stretches.
//
// Faults, 10 minutes after onset: compressor_failure +0.30, power_outage
// +0.15, door_left_open +0.10, sensor_drift -0.25 °C/min.
//
// At +0.10 a healthy cabinet crosses the line about once in 4,000 samples
// (a 30 s poll flickers roughly once per 33 hours), while compressor failure
// and power outage trip 60/60 and a door left open trips 30/60 — the rest
// surface through other signals (door events, rising mean). The original +0.05
// tripped 1.5% of healthy samples, which on a 12-device fleet meant a false
// AT_RISK about every 10 minutes.
const atRiskSlopeCPM = 0.10

// statusSwitch is the shared OK/AT_RISK/ALARM switch expression.
// OK: temp <= -14°C and not rising; AT_RISK: temp above -14°C, or rising
// faster than atRiskSlopeCPM; ALARM: temp above -10°C (spoilage). A missing
// slope is treated as flat, so a device with too few samples cannot alarm on
// absence.
func statusSwitch() bson.D {
	return bson.D{{Key: "$switch", Value: bson.D{
		{Key: "branches", Value: bson.A{
			bson.D{{Key: "case", Value: bson.D{{Key: "$gt", Value: bson.A{"$lastTemp", -10}}}},
				{Key: "then", Value: "ALARM"}},
			bson.D{{Key: "case", Value: bson.D{{Key: "$or", Value: bson.A{
				bson.D{{Key: "$gt", Value: bson.A{"$lastTemp", -14}}},
				bson.D{{Key: "$gt", Value: bson.A{bson.D{{Key: "$ifNull", Value: bson.A{"$lastSlope", 0}}}, atRiskSlopeCPM}}},
			}}}},
				{Key: "then", Value: "AT_RISK"}},
		}},
		{Key: "default", Value: "OK"},
	}}}
}

// recentStage bounds the pipeline to the last 15 minutes so the window
// functions never scan the full TTL window (24h of telemetry).
func recentStage() bson.D {
	return bson.D{{Key: "$match", Value: bson.D{
		{Key: "ts", Value: bson.M{"$gte": time.Now().UTC().Add(-15 * time.Minute)}},
	}}}
}

// deviceHealthPipeline returns the per-device health view:
//   - tempAvg5m: 5-minute rolling average via $setWindowFields (window functions)
//   - slopeCpm: temperature trend °C/minute via $derivative
func deviceHealthPipeline() bson.A {
	return bson.A{
		recentStage(),
		bson.D{{Key: "$sort", Value: bson.D{{Key: "ts", Value: 1}}}},
		slopeStage(),
		avgStage(),
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$meta.deviceId"},
			{Key: "lastTs", Value: bson.D{{Key: "$last", Value: "$ts"}}},
			{Key: "lastTemp", Value: bson.D{{Key: "$last", Value: "$tempC"}}},
			{Key: "avgTemp5m", Value: bson.D{{Key: "$last", Value: "$tempAvg5m"}}},
			{Key: "lastSlope", Value: bson.D{{Key: "$last", Value: "$slope"}}},
			{Key: "samples", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		bson.D{{Key: "$addFields", Value: bson.D{
			{Key: "status", Value: statusSwitch()},
		}}},
		bson.D{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 0},
			{Key: "deviceId", Value: "$_id"},
			{Key: "lastTs", Value: 1},
			{Key: "lastTemp", Value: 1},
			{Key: "avgTemp5m", Value: 1},
			{Key: "slopeCpm", Value: bson.D{{Key: "$round", Value: bson.A{bson.D{{Key: "$ifNull", Value: bson.A{"$lastSlope", 0}}}, 3}}}},
			{Key: "samples", Value: 1},
			{Key: "status", Value: 1},
		}}},
	}
}

// fleetRollupPipeline counts devices by status. Mirrored in
// datadog/agent/conf.d/mongo.d/conf.yaml custom_queries so the Agent emits
// `mongodb.coldchain.fleet.device_count` tagged by status.
func fleetRollupPipeline() bson.A {
	return bson.A{
		recentStage(),
		bson.D{{Key: "$sort", Value: bson.D{{Key: "ts", Value: 1}}}},
		slopeStage(),
		avgStage(),
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$meta.deviceId"},
			{Key: "lastTemp", Value: bson.D{{Key: "$last", Value: "$tempC"}}},
			{Key: "lastSlope", Value: bson.D{{Key: "$last", Value: "$slope"}}},
		}}},
		bson.D{{Key: "$addFields", Value: bson.D{
			{Key: "status", Value: statusSwitch()},
		}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$status"},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		bson.D{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 0},
			{Key: "status", Value: "$_id"},
			{Key: "count", Value: 1},
		}}},
	}
}

// DeviceHealthRow is one row of GET /devices/health.
type DeviceHealthRow struct {
	DeviceID  string    `bson:"deviceId"  json:"deviceId"`
	LastTs    time.Time `bson:"lastTs"    json:"lastTs"`
	LastTemp  float64   `bson:"lastTemp"  json:"lastTemp"`
	AvgTemp5m *float64  `bson:"avgTemp5m" json:"avgTemp5m"`
	SlopeCpm  *float64  `bson:"slopeCpm"  json:"slopeCpm"`
	Samples   int64     `bson:"samples"   json:"samples"`
	Status    string    `bson:"status"    json:"status"` // OK | AT_RISK | ALARM
}

// DeviceHealth runs the window-function health pipeline for one device or
// the whole fleet (deviceID == "" -> all devices).
func DeviceHealth(ctx context.Context, coll *mongo.Collection, deviceID string) ([]DeviceHealthRow, error) {
	pipeline := deviceHealthPipeline()
	if deviceID != "" {
		pipeline = append(bson.A{bson.D{{Key: "$match", Value: bson.D{{Key: "meta.deviceId", Value: deviceID}}}}}, pipeline...)
	}
	cur, err := coll.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	var out []DeviceHealthRow
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// FleetStatus is one {status -> count} row of the fleet rollup.
type FleetStatus struct {
	Status string  `bson:"status" json:"status"`
	Count  float64 `bson:"count"  json:"count"`
}

// SeriesPoint is one point of a device telemetry series (gap-free via
// $densify + $fill), used by the Ops Console charts.
type SeriesPoint struct {
	TS   string   `bson:"ts"   json:"ts"`
	Temp *float64 `bson:"temp" json:"temp"`
}

// DeviceSeries returns one device's readings for the last `window` duration,
// bucketed to 10-second bins and gap-filled. The $densify stage guarantees a
// continuous x-axis even when the device was offline (chart never lies).
func DeviceSeries(ctx context.Context, coll *mongo.Collection, deviceID string, window time.Duration) ([]SeriesPoint, error) {
	// Quantise the grid to the step so repeated polls describe the same bins:
	// a time.Now()-relative `to` makes every request drift a few hundred ms,
	// which shows up as x-axis jitter in the live chart.
	const step = 10 * time.Second
	to := time.Now().UTC().Truncate(step)
	from := to.Add(-window.Truncate(step))
	pipeline := bson.A{
		bson.D{{Key: "$match", Value: bson.D{{Key: "meta.deviceId", Value: deviceID},
			{Key: "ts", Value: bson.D{{Key: "$gte", Value: from}}}}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "ts", Value: 1}}}},
		// normalize the field name once: everything downstream uses "temp"
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "temp", Value: "$tempC"},
		}}},
		bson.D{{Key: "$densify", Value: bson.D{
			{Key: "field", Value: "ts"},
			{Key: "range", Value: bson.D{
				{Key: "step", Value: 10},
				{Key: "unit", Value: "second"},
				{Key: "bounds", Value: bson.A{from, to}},
			}},
		}}},
		// $fill writes ONLY into null bins, so a real reading is never
		// replaced by an interpolated value. `tempFront` covers bins before
		// the first sample (linear between the bounds) and `temp` carries
		// everything after it forward (locf). The projection then prefers the
		// real value, falling back to whichever filler applies — so genuine
		// excursions are plotted exactly as recorded instead of being
		// smoothed by an interpolated line.
		bson.D{{Key: "$fill", Value: bson.D{
			{Key: "sortBy", Value: bson.D{{Key: "ts", Value: 1}}},
			{Key: "output", Value: bson.D{
				{Key: "temp", Value: bson.D{{Key: "method", Value: "locf"}}},
				{Key: "tempFront", Value: bson.D{{Key: "method", Value: "linear"}}},
			}},
		}}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "temp", Value: bson.D{{Key: "$ifNull", Value: bson.A{"$temp", "$tempFront"}}}},
		}}},
		bson.D{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 0},
			{Key: "ts", Value: bson.D{{Key: "$toString", Value: "$ts"}}},
			{Key: "temp", Value: 1},
		}}},
	}
	cur, err := coll.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	var out []SeriesPoint
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// FleetStatus is one {status -> count} row of the fleet rollup (kept above).

// FleetRollup runs the fleet status-count pipeline (mirrored in
// datadog/agent/conf.d/mongo.d/conf.yaml custom_queries).
func FleetRollup(ctx context.Context, coll *mongo.Collection) ([]FleetStatus, error) {
	cur, err := coll.Aggregate(ctx, fleetRollupPipeline())
	if err != nil {
		return nil, err
	}
	var out []FleetStatus
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

var _ = collTelemetry
