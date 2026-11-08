package aggdb

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The dashboard flags a device AT_RISK when its slope exceeds atRiskSlopeCPM.
// That threshold is only meaningful if the slope averages over at least one
// thermostat cycle: a freezer pulls down quickly and then coasts for minutes,
// so a window shorter than a cycle reports whichever half it straddles — a
// perfectly healthy cabinet coasting reads a rising ~+0.1 °C/min and trips the
// alarm on its own normal behaviour.
//
// This test pins the window to 5 minutes. It is a constraint on the pipeline
// definition rather than on arithmetic, but it is the kind of detail that gets
// shortened during "cleanup" and silently breaks the fleet view.
func TestSlopeWindowCoversThermostatCycle(t *testing.T) {
	stage := slopeStage()

	var setWindow interface{}
	for _, f := range stage {
		if f.Key == "$setWindowFields" {
			setWindow = f.Value
		}
	}
	if setWindow == nil {
		t.Fatal("slopeStage has no $setWindowFields stage")
	}

	if got := slopeWindowMinutes(t, setWindow); got < 5 {
		t.Fatalf("slope window is %v minutes; need >= 5 so a thermostat cycle averages out", got)
	}
}

// TestStatusThresholdsMatchDocs keeps the ladder in aggdb in step with the
// documented rule (ALARM above -10, AT_RISK above -14 or rising).
func TestStatusThresholdsMatchDocs(t *testing.T) {
	var branches interface{}
	for _, f := range statusSwitch() {
		if f.Key == "$switch" {
			branches = f.Value
		}
	}
	if branches == nil {
		t.Fatal("statusSwitch has no $switch")
	}

	src := sprint(branches)
	for _, want := range []string{"ALARM", "AT_RISK"} {
		if !containsStr(src, want) {
			t.Errorf("status switch is missing the %s branch", want)
		}
	}
	for _, want := range []string{"-10", "-14"} {
		if !containsStr(src, want) {
			t.Errorf("status switch is missing threshold %s", want)
		}
	}
	if !containsStr(src, sprint(atRiskSlopeCPM)) {
		t.Errorf("status switch does not use atRiskSlopeCPM")
	}
}

// TestAtRiskSlopeClearsHealthyNoise pins the slope threshold between the two
// distributions it has to separate. Both numbers come from
// `go run ./tools/slopeprofile`, which drives the real device simulator:
//
//   - healthy, ~50k five-minute windows: p99.9 = +0.090, max = +0.105 °C/min
//   - compressor_failure and power_outage, 10 min after onset: +0.30 and
//     +0.15 °C/min
//
// A threshold at or below the healthy tail flickers a resting fleet yellow (the
// original +0.05 tripped 1.5% of healthy samples); one above the fault floor
// misses the faults the demo is built to catch.
func TestAtRiskSlopeClearsHealthyNoise(t *testing.T) {
	const (
		healthyP999CPM = 0.090 // healthy p99.9 — about one sample in a thousand
		faultFloorCPM  = 0.150 // power_outage, the slowest fault that ramps
	)

	if atRiskSlopeCPM <= healthyP999CPM {
		t.Errorf("atRiskSlopeCPM = %.3f is at or below the healthy p99.9 %.3f — resting freezers will flicker AT_RISK",
			atRiskSlopeCPM, healthyP999CPM)
	}
	if atRiskSlopeCPM >= faultFloorCPM {
		t.Errorf("atRiskSlopeCPM = %.3f is at or above the slowest fault ramp %.3f — real faults will not trip AT_RISK",
			atRiskSlopeCPM, faultFloorCPM)
	}
}

// TestDedupByTSFixesWindowPipelines pins the at-least-once delivery story.
// MQTT QoS 1 means a broker redelivery writes the same (deviceId, ts) twice;
// $setWindowFields/$fill REQUIRE the sortBy field to be unique per
// partition, so every window pipeline 500s with
// "There can be no repeated values in the sort field in the same partition"
// (observed live: one redelivery tick duplicated six devices and broke the
// series chart, the health view and the audit). Time-series collections
// cannot take unique indexes, so DedupByTS absorbs the duplicates on read.
// Integration test against real MongoDB (skipped when unreachable).
func TestDedupByTSFixesWindowPipelines(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mclient, db := testMongo(t)
	if mclient == nil {
		t.Skip("no mongodb reachable — run with the compose stack up")
	}
	defer func() { _ = mclient.Disconnect(ctx) }()

	coll := db.Collection("aggdb_dedup_test")
	_ = coll.Drop(ctx)
	defer func() { _ = coll.Drop(ctx) }()

	base := time.Now().UTC().Add(-time.Minute)
	doc := func(dev string, i int) bson.M {
		return bson.M{"meta": bson.M{"deviceId": dev}, "ts": base.Add(time.Duration(i) * 5 * time.Second), "tempC": -18 + 0.01*float64(i)}
	}
	// A has a redelivered duplicate at ts index 1 — byte-identical payload.
	// NB: the fixture is one device because $fill in DeviceSeries has no
	// partitionBy: the endpoint's $match narrows to one device first, and any
	// OTHER device sharing a ts would tie in the same (single) partition.
	docs := []any{doc("TEST-DUP-A", 0), doc("TEST-DUP-A", 1), doc("TEST-DUP-A", 1), doc("TEST-DUP-A", 2)}
	if _, err := coll.InsertMany(ctx, docs); err != nil {
		t.Fatalf("seed: %v", err)
	}
	matchA := bson.D{{Key: "$match", Value: bson.M{"meta.deviceId": "TEST-DUP-A"}}}

	// Without the dedup: $fill with method:linear — the DeviceSeries chart
	// stage — must hard-error on the duplicate sortBy value (the original
	// bug; locf tolerates ties, linear interpolation cannot).
	fillLinear := bson.D{{Key: "$fill", Value: bson.D{
		{Key: "sortBy", Value: bson.D{{Key: "ts", Value: 1}}},
		{Key: "output", Value: bson.D{{Key: "temp", Value: bson.D{{Key: "method", Value: "linear"}}}}},
	}}}
	raw := bson.A{matchA, fillLinear}
	cur, err := coll.Aggregate(ctx, raw)
	if err == nil {
		var out []bson.M
		err = cur.All(ctx, &out)
		cur.Close(ctx)
	}
	if err == nil {
		t.Fatal("duplicate ts in a partition did not error the window stage — the fixture no longer reproduces the bug")
	}

	// With DedupByTS: the same stage runs clean and the duplicate collapsed.
	fixed := append(bson.A{matchA}, DedupByTS()...)
	fixed = append(fixed, fillLinear)
	cur, err = coll.Aggregate(ctx, fixed)
	if err != nil {
		t.Fatalf("window stage with dedup errored: %v", err)
	}
	var out []bson.M
	if err := cur.All(ctx, &out); err != nil {
		t.Fatalf("window stage with dedup errored: %v", err)
	}
	cur.Close(ctx)
	if len(out) != 3 { // 3 unique readings (the duplicate collapsed)
		t.Fatalf("after dedup: %d rows, want 3 (the duplicate must collapse)", len(out))
	}
}

// TestDedupByTSShape pins the stage order: collapse by (deviceId, ts),
// re-root, re-sort — so a caller placing it before $setWindowFields can
// never lose the ts ordering $group destroys.
func TestDedupByTSShape(t *testing.T) {
	p := DedupByTS()
	if len(p) != 3 {
		t.Fatalf("DedupByTS has %d stages, want 3 (group/replaceRoot/sort)", len(p))
	}
	src := sprint(p)
	for _, want := range []string{`"$group"`, `"$replaceRoot"`, `"deviceId"`, `"$first"`, `"$sort"`} {
		if !containsStr(src, want) {
			t.Errorf("DedupByTS missing %s", want)
		}
	}
}
