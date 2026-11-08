// Package otlp — slog → OTel logs bridge. Adapts slog records to the
// otel/log API so JSON logs reach Datadog with severity and
// attributes, without changing any call site. Trace correlation comes free:
// sdk/log's Logger.Emit derives trace_id/span_id from the context.
package otlp

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// SlogBridge converts slog records into otel log records.
type SlogBridge struct {
	logger log.Logger
}

// NewSlogBridge builds a bridge bound to a logger provider.
func NewSlogBridge(lp *sdklog.LoggerProvider) *SlogBridge {
	if lp == nil {
		return nil
	}
	return &SlogBridge{logger: lp.Logger("app")}
}

// Handle forwards one slog record to the OTel log pipeline.
func (b *SlogBridge) Handle(ctx context.Context, r slog.Record) error {
	if b == nil || b.logger == nil {
		return nil
	}
	var severity log.Severity
	switch {
	case r.Level >= slog.LevelError:
		severity = log.SeverityError1
	case r.Level >= slog.LevelWarn:
		severity = log.SeverityWarn
	case r.Level == slog.LevelInfo:
		severity = log.SeverityInfo
	default:
		severity = log.SeverityDebug
	}

	rec := log.Record{}
	rec.SetTimestamp(r.Time)
	rec.SetSeverity(severity)
	rec.SetSeverityText(severityText(r.Level))
	rec.SetBody(attribute.StringValue(r.Message))
	r.Attrs(func(a slog.Attr) bool {
		rec.AddAttributes(slogAttrToKV(a))
		return true
	})
	b.logger.Emit(ctx, rec)
	return nil
}

func severityText(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN"
	case l == slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

// slogAttrToKV flattens slog values into OTel attributes.
func slogAttrToKV(a slog.Attr) attribute.KeyValue {
	switch a.Value.Kind() {
	case slog.KindString:
		return attribute.String(a.Key, a.Value.String())
	case slog.KindInt64:
		return attribute.Int64(a.Key, a.Value.Int64())
	case slog.KindBool:
		return attribute.Bool(a.Key, a.Value.Bool())
	case slog.KindFloat64:
		return attribute.Float64(a.Key, a.Value.Float64())
	case slog.KindDuration:
		return attribute.String(a.Key, a.Value.Duration().String())
	case slog.KindTime:
		return attribute.String(a.Key, a.Value.Time().UTC().String())
	default:
		return attribute.String(a.Key, a.Value.String())
	}
}
