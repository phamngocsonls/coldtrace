// Package ingest receives MQTT telemetry from the broker, persists it to the
// time series collection and emits observability signals:
//   - OTLP metrics (shipped to Datadog by the local otel-collector)
//   - structured JSON logs (collected by the stack's log pipeline)
//   - OTLP spans for the MQTT->Mongo pipeline (Distributed Tracing)
//
// On every reading it also upserts the device_state twin (a plain
// collection, watchable by change streams — time series views are not).
package ingest

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/eclipse/paho.golang/paho"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/onemount/coldtrace/internal/mqtttrace"
	"github.com/onemount/coldtrace/internal/telemetry"
)

// Ingester persists MQTT telemetry and emits OTLP signals.
type Ingester struct {
	log    *slog.Logger
	coll   mongoCollection
	twin   mongoCollection
	events mongoCollection
	tracer trace.Tracer

	// batcher buffers readings for insertMany (nil => direct per-reading
	// InsertOne, kept for unit tests and simple embedders).
	batcher *Batcher

	// devices is the registry used to fill the metaField labels (optional).
	devices    mongoCollection
	metaMu     sync.RWMutex
	metaCache  map[string]telemetry.Meta
	metaAt     time.Time
	metaLoaded bool

	tempGauge   metric.Float64Gauge
	powerGauge  metric.Float64Gauge
	latencyHist metric.Float64Histogram
	msgsCounter metric.Int64Counter
	failCounter metric.Int64Counter
	errsCounter metric.Int64Counter
}

type mongoCollection = *mongo.Collection

// New wires an Ingester against the telemetry time series collection.
func New(log *slog.Logger, coll mongoCollection) *Ingester {
	return NewWithTwin(log, coll, nil, nil)
}

// NewWithTwin wires an Ingester that also maintains the device_state twin
// and the events log (both may be nil in unit tests).
func NewWithTwin(log *slog.Logger, coll, twin, events mongoCollection) *Ingester {
	tracer := otel.Tracer("coldtrace.ingest")
	meter := otel.Meter("coldtrace.ingest")

	tempGauge, _ := meter.Float64Gauge("coldchain.device.temp",
		metric.WithDescription("Cabinet temperature (°C)"))
	powerGauge, _ := meter.Float64Gauge("coldchain.device.power_draw",
		metric.WithDescription("Compressor power draw (W)"))
	latencyHist, _ := meter.Float64Histogram("coldchain.ingest.latency",
		metric.WithDescription("MQTT->Mongo ingest latency (ms)"), metric.WithUnit("ms"))
	msgsCounter, _ := meter.Int64Counter("coldchain.ingest.messages",
		metric.WithDescription("Telemetry messages ingested"))
	failCounter, _ := meter.Int64Counter("coldchain.device.compressor_fail",
		metric.WithDescription("Compressor-not-running observations"))
	errsCounter, _ := meter.Int64Counter("coldchain.ingest.errors",
		metric.WithDescription("Ingest errors"))

	return &Ingester{
		log: log, coll: coll, twin: twin, events: events, tracer: tracer,
		tempGauge: tempGauge, powerGauge: powerGauge, latencyHist: latencyHist,
		msgsCounter: msgsCounter, failCounter: failCounter, errsCounter: errsCounter,
	}
}

// HandlePublish processes one inbound MQTT publish (paho router callback).
// Span: mqtt.ingest -> mongo.write (visible in Datadog APM).
// Acks on coldchain/{id}/acks are ignored here — the ops-api owns the
// command lifecycle; without this guard a SingleHandlerRouter would decode
// ack JSON as telemetry and write ghost tempC=0 rows into the TS collection.
func (ing *Ingester) HandlePublish(m *paho.Publish) {
	if strings.HasSuffix(m.Topic, "/acks") {
		return
	}
	// Trace continuation: devices embed a W3C traceparent in the payload —
	// device→gateway→AI agent become ONE distributed trace.
	var userProps map[string]string
	if m.Properties != nil && len(m.Properties.User) > 0 {
		userProps = make(map[string]string, len(m.Properties.User))
		for _, up := range m.Properties.User {
			userProps[up.Key] = up.Value
		}
	}
	ctx := mqtttrace.Extract(context.Background(), m.Payload, userProps)
	spanCtx, span := ing.tracer.Start(ctx, "mqtt.ingest",
		trace.WithAttributes(attribute.String("mqtt.topic", m.Topic)))
	start := time.Now()
	defer span.End()

	var t telemetry.Telemetry
	if err := json.Unmarshal(m.Payload, &t); err != nil {
		ing.log.Error("bad telemetry payload", "err", err, "topic", m.Topic)
		ing.errsCounter.Add(spanCtx, 1, metric.WithAttributes(attr("reason", "bad_payload")))
		return
	}
	devID, ok := deviceFromTopic(m.Topic)
	if !ok {
		ing.log.Error("non-conforming telemetry topic; dropping", "topic", m.Topic)
		ing.errsCounter.Add(spanCtx, 1, metric.WithAttributes(attr("reason", "bad_topic")))
		return
	}
	// metaField: the source id plus fixed labels. Populated from the device
	// registry so a site/zone filter can skip whole buckets.
	t.Meta = ing.metaFor(devID)
	t.TS = time.Now().UTC()
	span.SetAttributes(attribute.String("device.id", t.Meta.DeviceID))

	// Persist: batched insertMany when a Batcher is wired (the production
	// path), otherwise the legacy single-document write.
	//
	// Batching means there is no per-reading mongo.write child span any
	// more — one mongo.write_batch span covers the whole insertMany. The
	// enqueue itself is annotated on the mqtt.ingest span so a single
	// reading is still traceable end to end.
	if ing.batcher != nil {
		if err := ing.batcher.Add(spanCtx, t); err != nil {
			ing.log.Error("batch enqueue failed", "err", err, "deviceId", t.Meta.DeviceID)
			ing.errsCounter.Add(spanCtx, 1, metric.WithAttributes(attr("reason", "mongo")))
			return
		}
		span.SetAttributes(attribute.Bool("mongo.batched", true))
	} else {
		writeCtx, writeSpan := ing.tracer.Start(spanCtx, "mongo.write",
			trace.WithAttributes(
				attribute.String("db.operation", "insert_one"),
				attribute.String("db.system", "mongodb")))
		if _, err := ing.coll.InsertOne(writeCtx, t); err != nil {
			writeSpan.End()
			ing.log.Error("mongo insert failed", "err", err, "deviceId", t.Meta.DeviceID)
			ing.errsCounter.Add(spanCtx, 1, metric.WithAttributes(attr("reason", "mongo")))
			return
		}
		writeSpan.End()
	}

	// device twin upsert (best-effort; plain collection => change-streamable).
	// Stale-reading guard: QoS1 can deliver out of order — never let an older
	// reading overwrite a newer twin state. Done as UPDATE-only after a
	// find-modify: an upsert whose FILTER mentions reported.seq races the
	// unique deviceId index (E11000 on the insert side, seen live). A plain
	// $set with a seq comparison in the update pipeline is race-free and
	// always upserts the first reading.
	if ing.twin != nil {
		twinCtx, twinSpan := ing.tracer.Start(spanCtx, "mongo.twin_upsert")
		now := time.Now().UTC()
		next := bson.M{
			"ts": t.TS, "tempC": t.TempC, "powerDraw": t.PowerDraw,
			"compressorRunning": t.CompressorRunning, "doorOpen": t.DoorOpen, "seq": t.Seq,
		}
		res, err := ing.twin.UpdateOne(twinCtx,
			bson.M{"deviceId": t.Meta.DeviceID},
			bson.M{"$set": bson.M{
				"reported": next,
				"lastSeen": now,
				"deviceId": t.Meta.DeviceID,
				"staleAt":  now.Add(30 * time.Second),
			}},
			options.UpdateOne().SetUpsert(true))
		_ = res
		if err == nil {
			// second pass re-applies only when this reading is not older
			// than what is stored (covers the rare out-of-order arrival)
			ing.twin.UpdateOne(twinCtx,
				bson.M{"deviceId": t.Meta.DeviceID, "reported.seq": bson.M{"$gt": t.Seq}},
				bson.M{"$set": bson.M{"reported": next}})
		} else {
			ing.log.Warn("twin upsert failed", "deviceId", t.Meta.DeviceID, "err", err)
		}
		twinSpan.End()
	}

	latencyMs := float64(time.Since(start).Milliseconds())
	attrs := metric.WithAttributes(
		attr("device", t.Meta.DeviceID), attr("service", "coldtrace-gateway"))
	ing.tempGauge.Record(spanCtx, t.TempC, attrs)
	ing.powerGauge.Record(spanCtx, t.PowerDraw, attrs)
	ing.latencyHist.Record(spanCtx, latencyMs, attrs)
	ing.msgsCounter.Add(spanCtx, 1, attrs)
	if !t.CompressorRunning {
		ing.failCounter.Add(spanCtx, 1, attrs)
	}
	ing.log.Info("telemetry_ingested",
		"deviceId", t.Meta.DeviceID, "tempC", t.TempC, "compressorRunning", t.CompressorRunning,
		"latencyMs", latencyMs)
}

// deviceFromTopic extracts the device id from "coldchain/{id}/telemetry".
// The second return reports validity — readings from non-conforming topics
// must NOT be persisted (a deviceId "unknown" would pollute every aggregate).
func deviceFromTopic(topic string) (string, bool) {
	// topic: coldchain/{id}/telemetry
	const prefix = "coldchain/"
	s := topic
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		s = s[len(prefix):]
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], i > 0
		}
	}
	return "", false
}

func attr(k, v string) attribute.KeyValue { return attribute.String(k, v) }

// EnableBatching attaches a write Batcher built from cfg and starts it.
// The caller owns shutdown: call Close on the returned Batcher (the gateway
// defers it) so buffered readings are drained before exit.
func (ing *Ingester) EnableBatching(cfg BatchConfig, log *slog.Logger) *Batcher {
	b := NewBatcher(log, ing.coll, ing.tracer, otel.Meter("coldtrace.ingest"), cfg)
	b.Start()
	ing.batcher = b
	return b
}

// metaFor resolves the time series metaField for a device: the source id plus
// the fixed labels (site, zone) that make bucket-skipping possible.
//
// The registry is read once and cached — a lookup per reading would defeat the
// point of batching. A device missing from the registry still gets a valid
// meta with just its id, so ingest never depends on seeding order.
func (ing *Ingester) metaFor(deviceID string) telemetry.Meta {
	if ing.devices != nil {
		ing.metaMu.RLock()
		m, ok := ing.metaCache[deviceID]
		ing.metaMu.RUnlock()
		if ok {
			return m
		}
		if at, loaded := ing.metaAt, ing.metaLoaded; !loaded || time.Since(at) > 60*time.Second {
			ing.loadRegistry(context.Background())
			ing.metaMu.RLock()
			m, ok = ing.metaCache[deviceID]
			ing.metaMu.RUnlock()
			if ok {
				return m
			}
		}
	}
	return telemetry.Meta{DeviceID: deviceID}
}

// loadRegistry refreshes the deviceId -> {site, zone} cache.
func (ing *Ingester) loadRegistry(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cur, err := ing.devices.Find(ctx, bson.D{})
	if err != nil {
		return
	}
	defer func() { _ = cur.Close(ctx) }()
	next := make(map[string]telemetry.Meta, 16)
	var docs []struct {
		DeviceID string `bson:"deviceId"`
		Site     string `bson:"site"`
		Zone     string `bson:"zone"`
	}
	if err := cur.All(ctx, &docs); err != nil {
		return
	}
	for _, d := range docs {
		next[d.DeviceID] = telemetry.Meta{DeviceID: d.DeviceID, Site: d.Site, Zone: d.Zone}
	}
	ing.metaMu.Lock()
	ing.metaCache, ing.metaAt, ing.metaLoaded = next, time.Now(), true
	ing.metaMu.Unlock()
}

// WithDevices attaches the device registry so the metaField carries the
// device's fixed site/zone labels (bucket-skipping filters). Optional.
func (ing *Ingester) WithDevices(devices mongoCollection) *Ingester {
	ing.devices = devices
	ing.metaCache = make(map[string]telemetry.Meta, 16)
	return ing
}
