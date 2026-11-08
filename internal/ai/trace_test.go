package ai

import (
	"context"
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
