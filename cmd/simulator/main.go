// Command-line simulator for one virtual freezer device.
// Publishes JSON telemetry to coldchain/{id}/telemetry every
// SIM_INTERVAL_SECONDS (default 5s) and listens for commands on
// coldchain/{id}/commands.
//
// usage: simulator --device-id FZ-003 --broker mqtt://broker:1883
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/url"
	"os"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	"github.com/onemount/coldtrace/internal/sim"
)

func main() {
	var (
		deviceID = flag.String("device-id", "FZ-001", "device id")
		broker   = flag.String("broker", "mqtt://broker:1883", "mqtt broker url")
		interval = flag.Int("interval", 5, "publish interval seconds")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).
		With("deviceId", *deviceID, "service", "coldtrace-simulator")

	ctx := context.Background()
	cmdTopic := "coldchain/" + *deviceID + "/commands"
	telemetryTopic := "coldchain/" + *deviceID + "/telemetry"

	u, err := url.Parse(*broker)
	if err != nil {
		logger.Error("bad broker url", "err", err)
		os.Exit(1)
	}

	dev := sim.New(*deviceID, logger)

	cfg := autopaho.ClientConfig{
		BrokerUrls:        []*url.URL{u},
		KeepAlive:         30,
		ConnectRetryDelay: 2 * time.Second,
		ClientConfig: paho.ClientConfig{
			ClientID: "sim-" + *deviceID,
			OnServerDisconnect: func(*paho.Disconnect) {
				logger.Warn("broker disconnected; autopaho will retry")
			},
			OnClientError: func(err error) { logger.Warn("mqtt client error", "err", err) },
		},
	}
	cfg.ClientConfig.Router = paho.NewSingleHandlerRouter(func(m *paho.Publish) {
		var cmd struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(m.Payload, &cmd); err != nil {
			logger.Warn("bad command payload", "payload", string(m.Payload))
			return
		}
		switch cmd.Action {
		case "enter_compressor_failure":
			dev.SetFaultMode(true)
		case "compressor_restart":
			dev.SetFaultMode(false)
		default:
			logger.Warn("unknown command", "action", cmd.Action)
		}
	})
	cfg.OnConnectionUp = func(cm *autopaho.ConnectionManager, _ *paho.Connack) {
		if _, err := cm.Subscribe(ctx, &paho.Subscribe{
			Subscriptions: []paho.SubscribeOptions{{Topic: cmdTopic, QoS: 1}},
		}); err != nil {
			logger.Error("subscribe failed", "err", err)
		}
	}

	cm, err := autopaho.NewConnection(ctx, cfg)
	if err != nil {
		logger.Error("mqtt connection failed", "err", err)
		os.Exit(1)
	}
	if err := cm.AwaitConnection(ctx); err != nil {
		logger.Error("mqtt await connection failed", "err", err)
		os.Exit(1)
	}
	logger.Info("simulator online", "broker", *broker)

	t := time.NewTicker(time.Duration(*interval) * time.Second)
	defer t.Stop()
	for range t.C {
		r := dev.Tick()
		payload, err := json.Marshal(r)
		if err != nil {
			continue
		}
		if _, err := cm.Publish(ctx, &paho.Publish{
			Topic:   telemetryTopic,
			QoS:     1,
			Payload: payload,
		}); err != nil {
			logger.Warn("publish failed", "err", err)
		}
	}
}
