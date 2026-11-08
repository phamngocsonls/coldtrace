// Package mqtttrace propagates W3C trace context over MQTT so a reading
// forms ONE distributed trace: device (span) → gateway ingest → AI agent
// decision. Context rides in BOTH places for robustness:
//   - payload top-level "traceparent" field (always forwarded end to end)
//   - MQTT v5 user properties (clean for non-JSON consumers)
package mqtttrace

import (
	"context"
	"encoding/json"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

var propagator propagation.TextMapPropagator

func init() {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	propagator = otel.GetTextMapPropagator()
}

// payloadCarrier adapts a string map to the propagator API.
type payloadCarrier map[string]string

func (c payloadCarrier) Get(key string) string { return c[key] }
func (c payloadCarrier) Set(key, value string) { c[key] = value }
func (c payloadCarrier) Keys() []string        { return []string{"traceparent", "tracestate", "baggage"} }

// Inject stamps raw JSON payload bytes with the current trace context (both
// payload field and — when properties are non-nil — user properties).
// Non-JSON payloads pass through unchanged; extra fields are harmless to
// every consumer.
func Inject(ctx context.Context, payload []byte, props *PahoProps) []byte {
	var doc map[string]any
	isJSON := json.Unmarshal(payload, &doc) == nil
	carrier := payloadCarrier{}
	propagator.Inject(ctx, carrier)
	if len(carrier) == 0 {
		return payload
	}
	if props != nil && props.User == nil {
		props.User = map[string]string{}
	}
	for k, v := range carrier {
		if props != nil {
			props.User[k] = v
		}
		if isJSON {
			doc[k] = v
		}
	}
	if !isJSON {
		return payload
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return payload
	}
	return out
}

// PahoProps is the subset of paho.PublishProperties we touch, re-declared so
// this package stays importable from the simulator (which uses paho too —
// the types convert trivially at call sites).
type PahoProps struct {
	User map[string]string
}

// Extract rebuilds the trace context from an inbound MQTT publish: prefers
// the payload's traceparent, falls back to MQTT v5 user properties. Keys
// are matched case-insensitively (publishers may emit "Traceparent").
// Returns ctx unchanged when neither is present (the span starts a fresh
// trace).
func Extract(ctx context.Context, payload []byte, user map[string]string) context.Context {
	carrier := payloadCarrier{}
	if len(payload) > 0 {
		var doc map[string]any
		if err := json.Unmarshal(payload, &doc); err == nil {
			for k, v := range doc {
				if lk := strings.ToLower(k); isCtxKey(lk) {
					if s, ok := v.(string); ok && s != "" {
						carrier[lk] = s
					}
				}
			}
		}
	}
	for k, v := range user {
		if lk := strings.ToLower(k); isCtxKey(lk) && v != "" && carrier[lk] == "" {
			carrier[lk] = v
		}
	}
	if len(carrier) == 0 {
		return ctx
	}
	return propagator.Extract(ctx, carrier)
}

func isCtxKey(k string) bool {
	return k == "traceparent" || k == "tracestate" || k == "baggage"
}
