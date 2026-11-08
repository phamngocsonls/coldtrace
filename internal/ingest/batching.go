// Package ingest — batching.go implements the write batcher.
//
// Why: the MongoDB Time Series workshop brief (slide 13) is explicit —
// "Use insertMany. Hundreds of readings per call, not one insertOne each."
// One InsertOne per MQTT message costs a round trip and a bucket append per
// reading, and at fleet scale that is the most expensive thing the gateway
// does per message.
//
// Design:
//   - Readings are buffered and flushed when the buffer reaches MaxBatch or
//     when FlushInterval elapses, whichever comes first. A low-rate fleet
//     still lands data within ~one interval instead of waiting for a batch
//     that may never fill.
//   - Writes use InsertMany with { ordered: false } so one malformed document
//     cannot reject the rest of the batch (also from the brief).
//   - Failures are RETRIED with exponential backoff up to MaxAttempts. A
//     bounded retry queue was chosen over at-most-once because silently
//     dropping freezer readings would corrupt the trend analysis the product
//     depends on. Once attempts are exhausted the batch is dropped and
//     counted — bounded memory beats unbounded latency.
//   - Backpressure: Add blocks when the queue is full rather than growing
//     without limit. The MQTT callback is the only producer, so this slows
//     ingestion instead of eating memory.
//   - Ordering: records are flushed in arrival order, oldest first, which is
//     also what the brief asks for (out-of-order data works but compresses
//     worse).
package ingest

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/onemount/coldtrace/internal/telemetry"
)

// BatchConfig tunes the write batcher.
type BatchConfig struct {
	MaxBatch      int           // flush at this many readings
	FlushInterval time.Duration // ...or after this long, whichever is first
	MaxAttempts   int           // total insert attempts per batch (>=1)
	RetryBackoff  time.Duration // base backoff; doubles per attempt
	QueueSize     int           // capacity of the pending queue
}

// DefaultBatchConfig is tuned for a demo fleet: 12 devices @ 5 s = ~2.4
// readings/s, so the timer is what usually triggers a flush and writes stay
// perceptibly real-time while still coalescing bursts.
func DefaultBatchConfig() BatchConfig {
	return BatchConfig{
		MaxBatch:      100,
		FlushInterval: 250 * time.Millisecond,
		MaxAttempts:   5,
		RetryBackoff:  100 * time.Millisecond,
		QueueSize:     20000,
	}
}

// pending is one buffered reading plus the trace context it arrived with, so
// the mongo.write_batch span can still be correlated to the originating
// device→gateway trace.
type pending struct {
	reading telemetry.Telemetry
	ctx     context.Context
}

// Batcher buffers telemetry readings and writes them in batches.
type Batcher struct {
	cfg    BatchConfig
	log    *slog.Logger
	coll   *mongo.Collection
	tracer trace.Tracer

	in chan pending
	wg sync.WaitGroup

	flushed     metric.Int64Counter
	batches     metric.Int64Counter
	dropped     metric.Int64Counter
	retries     metric.Int64Counter
	batchSize   metric.Int64Histogram
	flushMillis metric.Float64Histogram

	closeOnce sync.Once
	done      chan struct{}
}

// NewBatcher creates a batcher. Call Start before Add, and Close on shutdown.
func NewBatcher(log *slog.Logger, coll *mongo.Collection, tracer trace.Tracer, meter metric.Meter, cfg BatchConfig) *Batcher {
	if cfg.MaxBatch <= 0 || cfg.FlushInterval <= 0 {
		cfg = DefaultBatchConfig()
	}
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	b := &Batcher{
		cfg: cfg, log: log, coll: coll, tracer: tracer,
		in:   make(chan pending, cfg.QueueSize),
		done: make(chan struct{}),
	}
	b.flushed, _ = meter.Int64Counter("coldchain.ingest.batch_flushed",
		metric.WithDescription("Telemetry readings written via batched insertMany"))
	b.batches, _ = meter.Int64Counter("coldchain.ingest.batch_writes",
		metric.WithDescription("Successful insertMany calls"))
	b.dropped, _ = meter.Int64Counter("coldchain.ingest.batch_dropped",
		metric.WithDescription("Readings dropped after exhausting retries"))
	b.retries, _ = meter.Int64Counter("coldchain.ingest.batch_retries",
		metric.WithDescription("Retry attempts made by the write batcher"))
	b.batchSize, _ = meter.Int64Histogram("coldchain.ingest.batch_size",
		metric.WithDescription("Readings per insertMany call"))
	b.flushMillis, _ = meter.Float64Histogram("coldchain.ingest.batch_flush_ms",
		metric.WithDescription("Wall time per batched write (ms)"), metric.WithUnit("ms"))
	return b
}

// Start launches the flush loop.
func (b *Batcher) Start() {
	b.wg.Add(1)
	go b.loop()
}

// Add enqueues a reading, blocking while the queue is full (backpressure).
//
// The closed-check is a separate non-blocking read BEFORE the send: a `select`
// with both a closed done channel and a buffered in channel picks at random,
// so a naive select would occasionally accept a write after Close. Checking
// first makes the common path correct; the select still unblocks a sender
// that is waiting on a full queue when Close fires.
func (b *Batcher) Add(ctx context.Context, t telemetry.Telemetry) error {
	select {
	case <-b.done:
		return errClosed{}
	default:
	}
	p := pending{reading: t, ctx: ctx}
	select {
	case <-b.done:
		return errClosed{}
	case b.in <- p:
		return nil
	}
}

// Close drains the queue, flushes everything buffered, then returns.
func (b *Batcher) Close() {
	b.closeOnce.Do(func() {
		close(b.done)
		b.wg.Wait()
	})
}

type errClosed struct{}

func (errClosed) Error() string { return "ingest: batcher is closed" }

func (b *Batcher) loop() {
	defer b.wg.Done()
	ticker := time.NewTicker(b.cfg.FlushInterval)
	defer ticker.Stop()

	buf := make([]pending, 0, b.cfg.MaxBatch)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		b.flush(buf)
		buf = buf[:0]
	}

	for {
		select {
		case p := <-b.in:
			buf = append(buf, p)
			if len(buf) >= b.cfg.MaxBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-b.done:
			// Drain what is already queued so a graceful shutdown does not
			// silently lose the last fraction of a second of readings.
			for {
				select {
				case p := <-b.in:
					buf = append(buf, p)
					if len(buf) >= b.cfg.MaxBatch {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// flush writes one batch with bounded retries.
func (b *Batcher) flush(buf []pending) {
	start := time.Now()
	n := len(buf)

	// The batch span hangs off the OLDEST reading's context so the write is
	// still part of a real inbound trace rather than a detached root.
	ctx, span := b.tracer.Start(buf[0].ctx, "mongo.write_batch",
		trace.WithAttributes(
			attribute.String("db.operation", "insert_many"),
			attribute.String("db.system", "mongodb"),
			attribute.String("db.collection", "telemetry"),
			attribute.Int("db.batch_size", n),
			attribute.Bool("db.ordered", false),
		))
	defer span.End()

	docs := make([]any, n)
	for i := range buf {
		docs[i] = buf[i].reading
	}

	var lastErr error
	for attempt := 1; attempt <= b.cfg.MaxAttempts; attempt++ {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, lastErr = b.coll.InsertMany(wctx, docs,
			// { ordered: false } — one malformed document must not reject the
			// rest of the batch (workshop brief). This REALLY sets it now: the
			// span attribute below claimed db.ordered:false while the call
			// above passed no options, leaving the driver default (true).
			options.InsertMany().SetOrdered(false))
		cancel()
		if lastErr == nil {
			b.flushed.Add(ctx, int64(n))
			b.batches.Add(ctx, 1)
			b.batchSize.Record(ctx, int64(n))
			b.flushMillis.Record(ctx, float64(time.Since(start).Milliseconds()))
			span.SetAttributes(attribute.Int("db.attempt", attempt))
			if attempt > 1 {
				b.log.Info("mongo_batch_recovered", "readings", n, "attempt", attempt)
			}
			return
		}

		span.RecordError(lastErr)
		if attempt < b.cfg.MaxAttempts {
			b.retries.Add(ctx, 1)
			backoff := b.cfg.RetryBackoff << (attempt - 1)
			b.log.Warn("mongo_batch_retry",
				"readings", n, "attempt", attempt, "backoff", backoff.String(), "err", lastErr)
			select {
			case <-time.After(backoff):
			case <-b.done:
				// Shutting down: stop retrying *now* instead of burning the
				// remaining attempts in a tight loop against a client that is
				// about to close. The batch is counted as dropped below.
				b.dropped.Add(ctx, int64(n))
				span.SetStatus(codes.Error, "batch dropped during shutdown")
				b.log.Warn("mongo_batch_dropped_shutdown", "readings", n, "err", lastErr)
				return
			}
		}
	}

	b.dropped.Add(ctx, int64(n))
	span.SetStatus(codes.Error, "batch dropped after retries")
	span.SetAttributes(attribute.Int("db.attempts_exhausted", b.cfg.MaxAttempts))
	b.log.Error("mongo_batch_dropped",
		"readings", n, "attempts", b.cfg.MaxAttempts, "err", lastErr)
}
