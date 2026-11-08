package ops

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestStatusExprIdleCompressorAtSetpoint pins the rule that a freezer resting
// at its setpoint with the compressor off is healthy.
//
// This was a live false positive on the demo fleet: the AT_RISK branch
// compared `$reported.tempC` against a top-level `$setpoint` that the twin
// never has (it lives at `desired.setpoint`), so the comparison ran against
// the -20 fallback. Every freezer sitting at -17.9 while off-cycle therefore
// counted as "above setpoint" and flickered AT_RISK for ~30 s at a time —
// visible to a judge as the fleet turning yellow on its own.
func TestStatusExprIdleCompressorAtSetpoint(t *testing.T) {
	now := time.Now().UTC()
	expr := statusExpr(now)

	cases := []struct {
		name   string
		doc    bson.M
		expect string
	}{
		{
			name: "resting at setpoint, compressor off -> OK",
			doc: bson.M{
				"lastSeen": now,
				"reported": bson.M{"tempC": -17.9, "compressorRunning": false},
				"desired":  bson.M{"setpoint": -18.0},
			},
			expect: StatusOK,
		},
		{
			// Just inside the deadband: a thermostat resting at +0.1 °C is
			// normal, and must not turn the fleet yellow.
			name: "just above setpoint, within deadband, compressor off -> OK",
			doc: bson.M{
				"lastSeen": now,
				"reported": bson.M{"tempC": -17.8, "compressorRunning": false},
				"desired":  bson.M{"setpoint": -18.0},
			},
			expect: StatusOK,
		},
		{
			// Just outside the deadband with the compressor off: the box is
			// genuinely not being cooled when it should be.
			name: "just outside deadband, compressor off -> AT_RISK",
			doc: bson.M{
				"lastSeen": now,
				"reported": bson.M{"tempC": -17.6, "compressorRunning": false},
				"desired":  bson.M{"setpoint": -18.0},
			},
			expect: StatusAtRisk,
		},
		{
			name: "warmer than setpoint, compressor off -> AT_RISK",
			doc: bson.M{
				"lastSeen": now,
				"reported": bson.M{"tempC": -16.0, "compressorRunning": false},
				"desired":  bson.M{"setpoint": -18.0},
			},
			expect: StatusAtRisk,
		},
		{
			name: "at setpoint with compressor running -> OK",
			doc: bson.M{
				"lastSeen": now,
				"reported": bson.M{"tempC": -18.0, "compressorRunning": true},
				"desired":  bson.M{"setpoint": -18.0},
			},
			expect: StatusOK,
		},
		{
			// The live miss that prompted this rule: FZ-003 was recovering
			// from a failed compressor, reading -14.4 °C with the compressor
			// running, and the fleet tile stayed green until -14. A running
			// compressor cannot cool a cabinet 3.6 °C above setpoint; the
			// gateway's slope view already called it AT_RISK.
			name: "compressor running but >2C above setpoint -> AT_RISK",
			doc: bson.M{
				"lastSeen": now,
				"reported": bson.M{"tempC": -14.4, "compressorRunning": true},
				"desired":  bson.M{"setpoint": -18.0},
			},
			expect: StatusAtRisk,
		},
		{
			name: "compressor running just outside warm band -> AT_RISK",
			doc: bson.M{
				"lastSeen": now,
				"reported": bson.M{"tempC": -15.9, "compressorRunning": true},
				"desired":  bson.M{"setpoint": -18.0},
			},
			expect: StatusAtRisk,
		},
		{
			// Between the tight deadband and the warm band, a running
			// compressor is doing its job (e.g. recovering from a defrost).
			name: "compressor running within warm band -> OK",
			doc: bson.M{
				"lastSeen": now,
				"reported": bson.M{"tempC": -16.5, "compressorRunning": true},
				"desired":  bson.M{"setpoint": -18.0},
			},
			expect: StatusOK,
		},
		{
			name: "above the hard threshold -> ALARM",
			doc: bson.M{
				"lastSeen": now,
				"reported": bson.M{"tempC": -8.0, "compressorRunning": true},
				"desired":  bson.M{"setpoint": -18.0},
			},
			expect: StatusAlarm,
		},
		{
			// The twin is written by ingest (reported only), so a device that
			// nobody has re-set has no `desired` subdocument. The registry's
			// top-level setpoint is the configured default and must still
			// catch a stopped compressor warming the box.
			name: "no desired, registry setpoint, compressor off -> AT_RISK",
			doc: bson.M{
				"lastSeen": now,
				"setpoint": -18.0,
				"reported": bson.M{"tempC": -16.9, "compressorRunning": false},
			},
			expect: StatusAtRisk,
		},
		{
			name: "no desired, registry setpoint, resting within deadband -> OK",
			doc: bson.M{
				"lastSeen": now,
				"setpoint": -18.0,
				"reported": bson.M{"tempC": -17.9, "compressorRunning": false},
			},
			expect: StatusOK,
		},
		{
			// Operator tightened the setpoint to -20: at -17 the box is warm
			// relative to the override, despite the looser registry default.
			name: "operator override wins over registry setpoint",
			doc: bson.M{
				"lastSeen": now,
				"setpoint": -16.0,
				"reported": bson.M{"tempC": -17.0, "compressorRunning": false},
				"desired":  bson.M{"setpoint": -20.0},
			},
			expect: StatusAtRisk,
		},
		{
			name: "no desired and no setpoint is unknown, not a fault -> OK",
			doc: bson.M{
				"lastSeen": now,
				"reported": bson.M{"tempC": -17.9, "compressorRunning": false},
			},
			expect: StatusOK,
		},
		{
			name: "stale heartbeat -> STALE",
			doc: bson.M{
				"lastSeen": now.Add(-5 * time.Minute),
				"reported": bson.M{"tempC": -18.0, "compressorRunning": true},
				"desired":  bson.M{"setpoint": -18.0},
			},
			expect: StatusStale,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalStatus(t, expr, tc.doc)
			if got != tc.expect {
				t.Fatalf("status = %s, want %s (doc %v)", got, tc.expect, tc.doc)
			}
		})
	}
}

// evalStatus applies the aggregation expression to a single document using
// MongoDB's own evaluation, so the test exercises the real operator semantics
// ($switch/$ifNull/missing-field behaviour) rather than a Go paraphrase.
func evalStatus(t *testing.T, expr bson.D, doc bson.M) string {
	t.Helper()
	out := runExpr(t, bson.A{
		bson.D{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 0},
			{Key: "status", Value: expr},
		}}},
	}, doc)
	return out
}
