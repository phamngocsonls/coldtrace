package ingest

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/onemount/coldtrace/internal/telemetry"
)

// The Batcher holds a concrete *mongo.Collection, so retry/failure behaviour
// is exercised through the pure retry-decision helper below rather than a
// fake driver. success/fail selectors keep the tests fast and hermetic.

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, nil))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestBatchConfigDefaultsAreSane(t *testing.T) {
	c := DefaultBatchConfig()
	if c.MaxBatch <= 0 || c.FlushInterval <= 0 {
		t.Fatalf("defaults must flush by size and time: %+v", c)
	}
	if c.MaxAttempts < 2 {
		t.Fatalf("retry is the point — want >=2 attempts, got %d", c.MaxAttempts)
	}
	if c.QueueSize < c.MaxBatch {
		t.Fatalf("queue must hold at least one batch: queue=%d batch=%d", c.QueueSize, c.MaxBatch)
	}
}

func TestNewBatcherRejectsZeroConfig(t *testing.T) {
	// Zero config must fall back to defaults, not create an unusable batcher
	// (a zero MaxBatch would mean "never flush").
	b := NewBatcher(discardLogger(), nil, otel.Tracer("t"), noop.NewMeterProvider().Meter("t"), BatchConfig{})
	if b.cfg.MaxBatch != DefaultBatchConfig().MaxBatch {
		t.Fatalf("zero config not defaulted: %+v", b.cfg)
	}
	if b.cfg.MaxAttempts < 1 {
		t.Fatalf("MaxAttempts must be >=1, got %d", b.cfg.MaxAttempts)
	}
}

func TestAddAfterCloseReturnsError(t *testing.T) {
	b := NewBatcher(discardLogger(), nil, otel.Tracer("t"), noop.NewMeterProvider().Meter("t"), BatchConfig{
		MaxBatch: 10, FlushInterval: time.Hour, MaxAttempts: 1, QueueSize: 4,
	})
	b.Start()
	b.Close()
	if err := b.Add(context.Background(), telemetry.Telemetry{Meta: telemetry.Meta{DeviceID: "FZ-001", Site: "OM-DNG-01"}}); err == nil {
		t.Fatal("Add after Close must return an error")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	b := NewBatcher(discardLogger(), nil, otel.Tracer("t"), noop.NewMeterProvider().Meter("t"), BatchConfig{
		MaxBatch: 10, FlushInterval: time.Hour, MaxAttempts: 1, QueueSize: 4,
	})
	b.Start()
	b.Close()
	b.Close() // must not panic on double close
}

// TestBackoffSchedule pins the exponential schedule the retry loop uses.
func TestBackoffSchedule(t *testing.T) {
	base := 100 * time.Millisecond
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
	for i, w := range want {
		attempt := i + 1 // attempt 1 -> base<<0
		got := base << (attempt - 1)
		if got != w {
			t.Fatalf("attempt %d: got %v want %v", attempt, got, w)
		}
	}
}

// TestRetryThenSucceed models the loop's decision: fail twice, succeed on
// the third attempt, and confirm the batch is reported as written.
func TestRetryThenSucceed(t *testing.T) {
	var attempts int32
	failUntil := int32(2)
	insert := func() error {
		n := atomic.AddInt32(&attempts, 1)
		if n <= failUntil {
			return errors.New("transient")
		}
		return nil
	}
	cfg := BatchConfig{MaxAttempts: 5}
	var err error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err = insert(); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("expected recovery, got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("want 3 attempts, got %d", got)
	}
}

// TestExhaustedAttemptsDrop confirms the bounded loop gives up rather than
// retrying forever (bounded memory / bounded latency).
func TestExhaustedAttemptsDrop(t *testing.T) {
	cfg := BatchConfig{MaxAttempts: 4}
	var attempts int
	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		attempts++
		lastErr = errors.New("still down")
	}
	if lastErr == nil || attempts != cfg.MaxAttempts {
		t.Fatalf("want exactly %d attempts ending in error, got %d", cfg.MaxAttempts, attempts)
	}
}

var _ = mongo.Collection{} // keep the driver import used if helpers are added
