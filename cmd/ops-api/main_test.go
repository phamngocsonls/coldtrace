package main

import (
	"os"
	"strings"
	"testing"
)

// TestAcksSubscriptionIsReconnectSafe pins the 2026-11-08 live incident:
// ops-api subscribed to device acks ONCE after the first connection, so a
// single broker TCP reset ("read: connection reset by peer") left the
// client publishing commands fine while never receiving another ack —
// every later command rotted to TIMEOUT even though the simulator logged
// its acks into the void. MQTT subscriptions are broker session state
// and every autopaho reconnect negotiates a clean session, so the
// subscription must live inside cfg.OnConnectionUp (fires on every
// reconnect, mirroring gateway/simulator/fleet-simulator) — never as a
// one-shot Subscribe after AwaitConnection. This is a source-shape test
// for the same reason the pipeline tests pin stage order: the wiring is
// the bug class, and it regresses silently.
func TestAcksSubscriptionIsReconnectSafe(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Skip("main.go not readable — run from the package directory")
	}
	s := string(src)

	up := strings.Index(s, "cfg.OnConnectionUp")
	if up < 0 {
		t.Fatal("ops-api must set cfg.OnConnectionUp — a one-shot Subscribe dies on the first broker reset (2026-11-08 incident)")
	}
	next := strings.Index(s[up:], "autopaho.NewConnection")
	if next < 0 {
		t.Fatal("OnConnectionUp must be set before the connection is created")
	}
	body := s[up : up+next]
	if !strings.Contains(body, `"coldchain/+/acks"`) {
		t.Fatal(`OnConnectionUp must subscribe "coldchain/+/acks"`)
	}
}
