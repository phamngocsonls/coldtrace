package ai

import (
	"context"
	"math"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func readAttrs(sd sdktrace.ReadOnlySpan) map[string]string {
	out := map[string]string{}
	for _, a := range sd.Attributes() {
		out[string(a.Key)] = a.Value.Emit()
	}
	return out
}

// spanRecorder is a synchronous SpanExporter that keeps spans in memory.
type spanRecorder struct{ spans []sdktrace.ReadOnlySpan }

func (r *spanRecorder) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	r.spans = append(r.spans, spans...)
	return nil
}
func (r *spanRecorder) Shutdown(context.Context) error { return nil }

// DecisionSpan must emit one span carrying the decision, its guardrail gates
// and the device context — that span is what makes coldtrace-ai-agent show up
// as its own node in Datadog APM.
func TestDecisionSpan(t *testing.T) {
	rec := new(spanRecorder)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(rec))
	defer tp.Shutdown(context.Background())

	tracer := tp.Tracer("coldtrace.ai")
	res := PolicyResult{
		Decision: DecisionAuto, Reason: "gates passed",
		ConfGate: true, EtaGate: true, Cooldown: false, Circuit: false,
	}
	_, err := DecisionSpan(context.Background(), tracer, DecisionSpanInput{
		DeviceID: "FZ-004", FaultClass: "compressor_failure",
		Classification: "AUTO", Confidence: 0.91, EtaAlarmMin: 8.5,
		AlarmingNow: 0, SimilarCount: 3, Result: res,
	})
	if err != nil {
		t.Fatalf("DecisionSpan: %v", err)
	}
	if len(rec.spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(rec.spans))
	}
	sd := rec.spans[0]
	if sd.Name() != "ai.decision" {
		t.Errorf("span name = %q, want ai.decision", sd.Name())
	}
	attrs := readAttrs(sd)
	want := map[string]string{
		"ai.device": "FZ-004", "ai.fault_class": "compressor_failure",
		"ai.decision": "AUTO", "ai.reason": "gates passed",
		"ai.confidence": "0.91", "ai.eta_alarm_min": "8.5",
		"ai.alarming_now": "0", "ai.similar_count": "3",
		"ai.conf_gate": "true", "ai.eta_gate": "true",
	}
	for k, v := range want {
		if attrs[k] != v {
			t.Errorf("attr %s = %q, want %q", k, attrs[k], v)
		}
	}
	// PROPOSED/HOLD are normal outcomes — never an error status
	if sd.Status().Code == codes.Error {
		t.Errorf("span must not be error status for non-AUTO outcomes")
	}

	// nil tracer must be a safe no-op (tracing is optional wiring)
	if id, err := DecisionSpan(context.Background(), nil, DecisionSpanInput{DeviceID: "X"}); err != nil || id != "" {
		t.Errorf("nil tracer should be a no-op, got id=%q err=%v", id, err)
	}
}

// A NaN ETA (device not on a rising path) must ride as the string "n/a",
// never as a float attribute. The Datadog exporter on the far side of the
// collector serializes attributes through JSON — a NaN float there drops the
// whole span from APM ("json: unsupported value: NaN"). This bit for real:
// sensor_drift windows always forecast NaN and their decision spans never
// reached Datadog.
func TestDecisionSpanNaNEtaIsString(t *testing.T) {
	rec := new(spanRecorder)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(rec))
	defer tp.Shutdown(context.Background())

	if _, err := DecisionSpan(context.Background(), tp.Tracer("coldtrace.ai"), DecisionSpanInput{
		DeviceID: "FZ-003", FaultClass: "sensor_drift",
		Classification: "PROPOSED", Confidence: 0.97,
		EtaAlarmMin: math.NaN(),
		Result:      PolicyResult{Decision: DecisionPropose, Reason: "first of class"},
	}); err != nil {
		t.Fatalf("DecisionSpan: %v", err)
	}
	attrs := readAttrs(rec.spans[0])
	if got := attrs["ai.eta_alarm_min"]; got != "n/a" {
		t.Errorf("ai.eta_alarm_min = %q, want \"n/a\" (NaN must not be a float attribute)", got)
	}
	// and a normal ETA still renders numerically
	rec2 := new(spanRecorder)
	tp2 := sdktrace.NewTracerProvider(sdktrace.WithSyncer(rec2))
	defer tp2.Shutdown(context.Background())
	_, _ = DecisionSpan(context.Background(), tp2.Tracer("t"), DecisionSpanInput{
		DeviceID: "d", EtaAlarmMin: 12.5, Result: PolicyResult{Decision: DecisionAuto},
	})
	if got := readAttrs(rec2.spans[0])["ai.eta_alarm_min"]; got != "12.5" {
		t.Errorf("normal ETA rendered as %q, want 12.5", got)
	}
}
