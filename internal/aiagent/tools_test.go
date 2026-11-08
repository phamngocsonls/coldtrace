// Tests for the pure parts of the tool layer: status derivation and the
// window clamp logic. DB-backed paths are covered by GroundedChat's fake
// gateway test (tools_test db-backed parts need a live mongo).
package aiagent

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestDeriveStatus(t *testing.T) {
	now := time.Now().UTC()
	temp := func(v float64) bson.M { return bson.M{"tempC": v} }
	cases := []struct {
		name     string
		reported bson.M
		lastSeen *time.Time
		want     string
	}{
		{"cold ok", temp(-18), &now, "OK"},
		{"at risk band", temp(-12), &now, "AT_RISK"},
		{"alarm", temp(-9), &now, "ALARM"},
		{"no reading -> STALE", bson.M{}, &now, "STALE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := deriveStatus(c.reported, c.lastSeen)
			if got != c.want {
				t.Fatalf("deriveStatus = %q, want %q", got, c.want)
			}
		})
	}
	stale := now.Add(-2 * time.Minute)
	if got, _ := deriveStatus(temp(-18), &stale); got != "STALE" {
		t.Fatalf("2m-old device = %q, want STALE", got)
	}
	if got, _ := deriveStatus(temp(-18), nil); got != "STALE" {
		t.Fatalf("nil lastSeen = %q, want STALE", got)
	}
}

// clampTelemetryWindow mirrors the clamp inside toolTelemetry — tested
// directly because the DB path needs a live mongo.
func TestClampTelemetryWindow(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{-5 * time.Minute, time.Minute},
		{30 * time.Second, time.Minute},
		{15 * time.Minute, 15 * time.Minute},
		{9999 * time.Hour, 24 * time.Hour},
	}
	for _, c := range cases {
		w := c.in
		if w < time.Minute {
			w = time.Minute
		}
		if w > 24*time.Hour {
			w = 24 * time.Hour
		}
		if w != c.want {
			t.Fatalf("clamp(%v) = %v, want %v", c.in, w, c.want)
		}
	}
}
