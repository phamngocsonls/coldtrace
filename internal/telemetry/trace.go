// Package telemetry — device-side span helpers. The fleet simulator is not
// a full OTLP exporter (12 devices × HTTP exporter would multiply the demo
// footprint); instead each reading opens a tiny local span purely to mint a
// valid W3C traceparent, which rides the MQTT payload into the gateway.
// The gateway's mqtt.ingest span continues that trace — one E2E trace per
// reading across device → gateway → AI agent.
package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// StartTraceSpan opens a "device.reading" span for one device reading and
// returns the context carrying it (pass it to mqtttrace.Inject) plus the
// span (call End after publishing).
func StartTraceSpan(ctx context.Context, deviceID string) (context.Context, trace.Span) {
	tr := otel.Tracer("coldtrace.device")
	return tr.Start(ctx, "device.reading",
		trace.WithAttributes(attribute.String("device.id", deviceID)))
}
