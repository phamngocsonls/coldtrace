// Package ai — OTel decision spans. One span per ai.Evaluate outcome gives
// coldtrace-ai-agent its own node + decision timeline in Datadog APM
// without coupling the policy code to a vendor SDK.
package ai

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// DecisionSpanInput carries everything the decision span should record.
type DecisionSpanInput struct {
	DeviceID       string
	FaultClass     string
	Classification string // AUTO / PROPOSED / HOLD (display value)
	Confidence     float64
	EtaAlarmMin    float64
	AlarmingNow    int
	SimilarCount   int
	TopFactors     []TopFactor
	Result         PolicyResult
}

// DecisionSpan emits the "ai.decision" span and returns its trace ID
// (hex, 32 chars) so the incident record can deep-link into Datadog.
// A nil tracer is a safe no-op — tracing stays optional wiring, never a
// policy dependency.
func DecisionSpan(ctx context.Context, tracer trace.Tracer, in DecisionSpanInput) (string, error) {
	if tracer == nil {
		return "", nil
	}
	attrs := []attribute.KeyValue{
		attribute.String("ai.device", in.DeviceID),
		attribute.String("ai.fault_class", in.FaultClass),
		attribute.String("ai.decision", in.Classification),
		attribute.String("ai.reason", in.Result.Reason),
		attribute.Float64("ai.confidence", in.Confidence),
		attribute.Float64("ai.eta_alarm_min", in.EtaAlarmMin),
		attribute.Int("ai.alarming_now", in.AlarmingNow),
		attribute.Int("ai.similar_count", in.SimilarCount),
		attribute.Bool("ai.conf_gate", in.Result.ConfGate),
		attribute.Bool("ai.eta_gate", in.Result.EtaGate),
		attribute.Bool("ai.cooldown", in.Result.Cooldown),
		attribute.Bool("ai.circuit_breaker", in.Result.Circuit),
	}
	// explainability: top factor contributions as span attributes
	for i, f := range in.TopFactors {
		if i >= 3 {
			break
		}
		attrs = append(attrs,
			attribute.String("ai.top_factor."+string(rune('1'+i))+".name", f.Feature),
			attribute.Float64("ai.top_factor."+string(rune('1'+i))+".contribution", f.Contribution),
			attribute.String("ai.top_factor."+string(rune('1'+i))+".direction", f.Direction),
		)
	}
	_, span := tracer.Start(ctx, "ai.decision", trace.WithAttributes(attrs...))
	defer span.End()

	var traceID string
	if sc := span.SpanContext(); sc.IsValid() {
		traceID = sc.TraceID().String()
	}
	return traceID, nil
}
