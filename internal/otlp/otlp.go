// Package otlp wires OTLP export for traces, metrics AND logs.
// Backend is the local otel-collector, which ships to Datadog with the
// native exporter (otelcol/config.yaml) — see docker-compose.yml.
//
// Never block startup: exporters dial in the background; a short connect
// timeout keeps the first export attempt from hanging for minutes.
package otlp

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	dialTimeout    = 5 * time.Second  // TCP connect timeout only
	exportTimeout  = 30 * time.Second // per-export deadline (large batches)
	exportInterval = 15 * time.Second
)

// endpointFromEnv splits OTEL_EXPORTER_OTLP_ENDPOINT ("http://host:4318")
// into the host:port the otlphttp exporters expect, and reports whether
// TLS was requested via the scheme (https:// → secure).
func endpointFromEnv() (string, bool) {
	ep := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if ep == "" {
		return "otel-collector:4318", false
	}
	secure := strings.HasPrefix(ep, "https://")
	ep = strings.TrimPrefix(ep, "https://")
	ep = strings.TrimPrefix(ep, "http://")
	return ep, secure
}

// traceOpts / metricOpts / logOpts build non-blocking exporter options.
func traceOpts(addr string, secure bool) []otlptracehttp.Option {
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(addr),
		otlptracehttp.WithTimeout(exportTimeout),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{
			Enabled:         true,
			InitialInterval: 2 * time.Second,
			MaxInterval:     15 * time.Second,
			MaxElapsedTime:  0, // retry forever in the background
		}),
	}
	if !secure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	return opts
}

func metricOpts(addr string, secure bool) []otlpmetrichttp.Option {
	opts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpoint(addr),
		otlpmetrichttp.WithTimeout(exportTimeout),
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{
			Enabled:         true,
			InitialInterval: 2 * time.Second,
			MaxInterval:     15 * time.Second,
			MaxElapsedTime:  0,
		}),
	}
	if !secure {
		opts = append(opts, otlpmetrichttp.WithInsecure())
	}
	return opts
}

func logOpts(addr string, secure bool) []otlploghttp.Option {
	opts := []otlploghttp.Option{
		otlploghttp.WithEndpoint(addr),
		otlploghttp.WithTimeout(exportTimeout),
		otlploghttp.WithRetry(otlploghttp.RetryConfig{
			Enabled:         true,
			InitialInterval: 2 * time.Second,
			MaxInterval:     15 * time.Second,
			MaxElapsedTime:  0,
		}),
	}
	if !secure {
		opts = append(opts, otlploghttp.WithInsecure())
	}
	return opts
}

// Res builds the shared resource: service name + version + environment
// (Datadog unified service tagging compatible).
func Res(serviceName string) *resource.Resource {
	v := os.Getenv("SERVICE_VERSION")
	if v == "" {
		v = "dev"
	}
	return resource.NewWithAttributes(
		"https://opentelemetry.io/schemas/1.26.0",
		attribute.String("service.name", serviceName),
		attribute.String("service.version", v),
		attribute.String("deployment.environment", envOrDefault("DD_ENV", "hackathon")),
	)
}

// StartTrace configures the global trace provider (OTLP HTTP, /v1/traces).
func StartTrace(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	addr, secure := endpointFromEnv()
	exporter, err := otlptracehttp.New(ctx, traceOpts(addr, secure)...)
	if err != nil {
		return nil, err
	}
	tp := trace.NewTracerProvider(
		trace.WithBatcher(exporter),
		trace.WithResource(Res(serviceName)),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// StartMetric configures the global meter provider (OTLP HTTP, /v1/metrics).
func StartMetric(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	addr, secure := endpointFromEnv()
	exporter, err := otlpmetrichttp.New(ctx, metricOpts(addr, secure)...)
	if err != nil {
		return nil, err
	}
	mp := metric.NewMeterProvider(
		metric.WithReader(metric.NewPeriodicReader(exporter, metric.WithInterval(exportInterval))),
		metric.WithResource(Res(serviceName)),
	)
	otel.SetMeterProvider(mp)
	return mp.Shutdown, nil
}

// StartLogs configures the global log provider (OTLP HTTP, /v1/logs) and
// returns (shutdown, loggerProvider). The bridge handler from NewTraceLogger
// must be added to the returned provider for slog records to flow.
func StartLogs(ctx context.Context, serviceName string) (func(context.Context) error, *sdklog.LoggerProvider, error) {
	addr, secure := endpointFromEnv()
	exporter, err := otlploghttp.New(ctx, logOpts(addr, secure)...)
	if err != nil {
		return nil, nil, err
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
		sdklog.WithResource(Res(serviceName)),
	)
	return lp.Shutdown, lp, nil
}

// NewTraceLogger wraps slog's JSON handler so every record carries the
// active trace_id/span_id — log-trace correlation in Datadog.
func NewTraceLogger(base *slog.Logger, lp *sdklog.LoggerProvider) *slog.Logger {
	if lp == nil {
		return base
	}
	bridge := NewSlogBridge(lp)
	return slog.New(&traceHandler{next: base.Handler(), bridge: bridge})
}

type traceHandler struct {
	next   slog.Handler
	bridge *SlogBridge
}

func (h *traceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

// Handle decorates the record with trace IDs (when a span is active) and
// forwards it both to stdout and the OTel log bridge.
func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := oteltrace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	if err := h.next.Handle(ctx, r.Clone()); err != nil {
		return err
	}
	return h.bridge.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{next: h.next.WithAttrs(attrs), bridge: h.bridge}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{next: h.next.WithGroup(name), bridge: h.bridge}
}

func envOrDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
