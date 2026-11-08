// ColdTrace fleet simulator — one process, many devices.
//
// Each device runs the same physics as the single-device simulator
// (internal/sim), publishes telemetry to coldchain/{id}/telemetry and
// listens for commands on coldchain/{id}/commands. Command acks are
// published to coldchain/{id}/acks so the ops-api can track the full
// command lifecycle (SENT -> ACKED with latency).
//
// usage: fleet-simulator --devices FZ-001..FZ-012 --broker mqtt://broker:1883
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"

	"github.com/onemount/coldtrace/internal/mqtttrace"
	"github.com/onemount/coldtrace/internal/otlp"
	"github.com/onemount/coldtrace/internal/sim"
	"github.com/onemount/coldtrace/internal/telemetry"
)

func main() {
	var (
		deviceSpec = flag.String("devices", "FZ-001..FZ-012", "device ids: FZ-001..FZ-012 or comma list")
		broker     = flag.String("broker", env("MQTT_BROKER", "mqtt://broker:1883"), "mqtt broker url")
		interval   = flag.Int("interval", envInt("SIM_INTERVAL_SECONDS", 5), "publish interval seconds")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "coldtrace-fleet-simulator")
	ctx := context.Background()

	// Mint valid W3C traceparents per reading: a real (exporting) tracer
	// provider is required — without an SDK the span context is empty and
	// the payload carries no traceparent (broken trace continuation).
	stopTrace, err := otlp.StartTrace(ctx, "coldtrace-fleet-simulator")
	if err != nil {
		logger.Warn("otlp trace init", "err", err)
	}
	defer func() {
		if stopTrace != nil {
			shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = stopTrace(shCtx)
		}
	}()

	ids, err := parseDevices(*deviceSpec)
	if err != nil {
		logger.Error("bad --devices spec", "err", err)
		os.Exit(1)
	}

	// one MQTT connection for the whole fleet (client id unique per process)
	clientID := "coldtrace-fleet-" + fmt.Sprint(os.Getpid())
	u, err := url.Parse(*broker)
	if err != nil {
		logger.Error("bad broker url", "err", err)
		os.Exit(1)
	}

	type device struct {
		sim  *sim.Device
		mu   sync.Mutex
		last sim.Reading
	}
	devices := map[string]*device{}
	for _, id := range ids {
		devices[id] = &device{sim: sim.New(id, logger.With("deviceId", id))}
	}
	var fleetMu sync.Mutex

	cfg := autopaho.ClientConfig{
		BrokerUrls:        []*url.URL{u},
		KeepAlive:         30,
		ConnectRetryDelay: 2 * time.Second,
		ClientConfig: paho.ClientConfig{
			ClientID: clientID,
			OnServerDisconnect: func(*paho.Disconnect) {
				logger.Warn("broker disconnected; autopaho will retry")
			},
			OnClientError: func(err error) { logger.Warn("mqtt client error", "err", err) },
		},
	}

	// Router: telemetry on coldchain/+/telemetry; commands + acks per device.
	// paho's SingleHandlerRouter is one-handler-only; use a standard router.
	var cmRef atomic.Pointer[autopaho.ConnectionManager] // router goroutine reads; main writes
	cfg.ClientConfig.Router = paho.NewStandardRouter()
	cfg.OnConnectionUp = func(cm *autopaho.ConnectionManager, _ *paho.Connack) {
		if _, err := cm.Subscribe(ctx, &paho.Subscribe{
			Subscriptions: []paho.SubscribeOptions{
				{Topic: "coldchain/+/commands", QoS: 1},
			},
		}); err != nil {
			logger.Error("subscribe failed", "err", err)
		}
	}
	cfg.ClientConfig.Router.RegisterHandler("coldchain/+/commands", func(m *paho.Publish) {
		// fleet-wide broadcast resets every device in this process
		if m.Topic == "coldchain/fleet/commands" {
			var cmd struct {
				Action string `json:"action"`
			}
			if err := json.Unmarshal(m.Payload, &cmd); err == nil {
				switch sim.Action(cmd.Action) {
				case sim.ActionCompressorRestart:
					for _, d := range devices {
						d.sim.SetFault(sim.FaultNone)
					}
					logger.Info("fleet_reset_applied", "devices", len(devices))
				case sim.ActionDemoReset:
					for _, d := range devices {
						d.sim.Reset()
					}
					logger.Info("fleet_demo_reset_applied", "devices", len(devices), "detail", "faults cleared, cabinets at setpoint")
				}
			}
			return
		}
		deviceID := segment(m.Topic, 1)
		d := devices[deviceID]
		if d == nil {
			return // not our device
		}
		var cmd struct {
			ID     string          `json:"id"`
			Action string          `json:"action"`
			Args   json.RawMessage `json:"args,omitempty"`
		}
		if err := json.Unmarshal(m.Payload, &cmd); err != nil {
			logger.Warn("bad command payload", "deviceId", deviceID, "err", err)
			return
		}
		ack := d.sim.HandleCommand(cmd.ID, sim.Action(cmd.Action), cmd.Args)
		if cm := cmRef.Load(); cm != nil {
			payload, _ := json.Marshal(ack)
			_, _ = cm.Publish(context.Background(), &paho.Publish{
				Topic:   "coldchain/" + deviceID + "/acks",
				QoS:     1,
				Payload: payload,
			})
		}
		logger.Info("command_ack", "deviceId", deviceID, "action", cmd.Action,
			"commandId", ack.CommandID, "ok", ack.OK, "detail", ack.Detail)
	})

	cm, err := autopaho.NewConnection(ctx, cfg)
	if err != nil {
		logger.Error("mqtt connection failed", "err", err)
		os.Exit(1)
	}
	if err := cm.AwaitConnection(ctx); err != nil {
		logger.Error("mqtt await connection failed", "err", err)
		os.Exit(1)
	}
	cmRef.Store(cm)
	logger.Info("fleet online", "broker", *broker, "devices", len(ids), "interval", *interval)

	t := time.NewTicker(time.Duration(*interval) * time.Second)
	defer t.Stop()
	for range t.C {
		fleetMu.Lock()
		for id, d := range devices {
			r := d.sim.Tick()
			// Each reading starts a fresh trace; the W3C traceparent rides in
			// the payload so gateway ingest + AI decision continue the chain.
			traceCtx, span := telemetry.StartTraceSpan(ctx, id)
			payload, err := json.Marshal(r)
			if err == nil {
				payload = mqtttrace.Inject(traceCtx, payload, nil)
			}
			span.End()
			if err != nil {
				continue
			}
			if _, err := cm.Publish(ctx, &paho.Publish{
				Topic:   "coldchain/" + id + "/telemetry",
				QoS:     1,
				Payload: payload,
			}); err != nil {
				logger.Warn("publish failed", "deviceId", id, "err", err)
			}
		}
		fleetMu.Unlock()
	}
}

// parseDevices accepts "FZ-001..FZ-012" or "FZ-001,FZ-002".
func parseDevices(spec string) ([]string, error) {
	if s := strings.TrimSpace(spec); strings.Contains(s, "..") {
		parts := strings.SplitN(s, "..", 2)
		start, stop := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		prefix := start[:len(start)-3] // letters before the 3-digit suffix
		from, err := strconv.Atoi(start[len(start)-3:])
		if err != nil {
			return nil, fmt.Errorf("bad range start %q", start)
		}
		to, err := strconv.Atoi(stop[len(stop)-3:])
		if err != nil || to < from {
			return nil, fmt.Errorf("bad range end %q", stop)
		}
		var out []string
		for i := from; i <= to; i++ {
			out = append(out, fmt.Sprintf("%s%03d", prefix, i))
		}
		return out, nil
	}
	var out []string
	for _, s := range strings.Split(spec, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// segment returns the n-th (0-based) path segment of an MQTT topic.
func segment(topic string, n int) string {
	segs := strings.Split(topic, "/")
	if n < len(segs) {
		return segs[n]
	}
	return ""
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
