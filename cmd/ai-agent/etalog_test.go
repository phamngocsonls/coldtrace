package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"testing"
	"time"
)

// The slog → OTel bridge ships these records to the collector, whose Datadog
// logs exporter serializes them through JSON and DROPS any record containing
// a NaN float ("json: unsupported value: NaN"). Before this helper existed,
// every ai_decision / ai_decision_refreshed line for a device with no rising
// path (sensor_drift) vanished from Log Explorer.
func TestEtaLogAttrNeverEmitsNaN(t *testing.T) {
	// NaN renders as the string "n/a", never as a float.
	a := etaLogAttr(math.NaN())
	if a.Key != "etaAlarmMin" {
		t.Fatalf("key = %q, want etaAlarmMin", a.Key)
	}
	if got := a.Value.String(); got != "n/a" {
		t.Fatalf("NaN rendered as %q, want \"n/a\"", got)
	}

	// A real ETA still renders numerically.
	if got := etaLogAttr(12.34).Value.String(); got != "12.34" {
		t.Fatalf("normal ETA rendered as %q, want 12.34", got)
	}

	// End-to-end proof: a log record built with the helper must survive a
	// JSON marshal — the exact serialization the Datadog exporter does.
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, nil)
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "ai_decision", 0)
	rec.AddAttrs(slog.String("deviceId", "FZ-003"), etaLogAttr(math.NaN()))
	h.Handle(context.Background(), rec)
	var v map[string]any
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("log record is not valid JSON: %v — %s", err, buf.String())
	}
	if v["etaAlarmMin"] != "n/a" {
		t.Fatalf("etaAlarmMin = %v, want \"n/a\"", v["etaAlarmMin"])
	}

	// The drop happens DOWNSTREAM of slog: the OTel bridge converts a
	// KindFloat64 attr to an OTLP double, and the collector's Datadog
	// logs exporter then fails "json: unsupported value: NaN" and drops
	// the whole record. So the property to pin is the VALUE KIND: the
	// helper emits a string for NaN, the old shape emitted the float.
	if got := etaLogAttr(math.NaN()).Value.Kind(); got != slog.KindString {
		t.Fatalf("NaN attr kind = %v, want KindString (strings never hit the NaN drop)", got)
	}
	if old := slog.Float64("etaAlarmMin", math.NaN()).Value.Kind(); old != slog.KindFloat64 {
		t.Fatalf("sanity: raw slog.Float64 kind = %v, want KindFloat64", old)
	}
	if got := etaLogAttr(12.34).Value.Kind(); got != slog.KindFloat64 {
		t.Fatalf("normal ETA kind = %v, want KindFloat64", got)
	}
}
