package seed

import (
	"testing"
	"time"

	"github.com/onemount/coldtrace/internal/ai"
)

// The AI agent's incident memory (ai.IncidentMemory)
// scans RESOLVED incidents and cosine-compares their `features`. On a fresh
// stack there are none — the "similar past incidents" panel renders empty
// on stage, which reads as a broken feature. The seed package exists so
// the demo starts with a history that is CONSISTENT with the live path:
// same feature extractor, same normalization, same fault classes.

// TestIncidentsAreResolvedAndReal pins the shape of seeded incidents.
func TestIncidentsAreResolvedAndReal(t *testing.T) {
	now := time.Now().UTC()
	inc := Incidents(24, now)

	if len(inc) != 24 {
		t.Fatalf("got %d incidents, want 24", len(inc))
	}
	seenDevices := map[string]bool{}
	perClass := map[string]int{}
	for _, i := range inc {
		if i.Status != "RESOLVED" {
			t.Errorf("%s status = %s, want RESOLVED (incident memory only scans resolved)", i.DeviceID, i.Status)
		}
		if i.OpenedAt.After(now) || i.RemediatedAt.Before(i.OpenedAt) {
			t.Errorf("%s: openedAt %v not before now/remediatedAt %v", i.DeviceID, i.OpenedAt, i.RemediatedAt)
		}
		if len(i.Features) != ai.NumFeatures {
			t.Fatalf("%s: %d features, want %d (Normalize(Features(w)) dim)", i.DeviceID, len(i.Features), ai.NumFeatures)
		}
		if !validClass(i.FaultClass) {
			t.Errorf("faultClass %q not in ai.Classes", i.FaultClass)
		}
		if i.DeviceID == "" || !validDeviceID(i.DeviceID) {
			t.Errorf("deviceId %q not a fleet device", i.DeviceID)
		}
		if i.Decision != "AUTO" {
			t.Errorf("seeded RESOLVED incident decision = %s, want AUTO (it was remediated)", i.Decision)
		}
		if !i.Seeded {
			t.Error("seeded incidents must carry seeded:true so re-seeding can replace them")
		}
		if i.Confidence < 0.5 || i.Confidence > 1 {
			t.Errorf("confidence %v out of range", i.Confidence)
		}
		seenDevices[i.DeviceID] = true
		perClass[i.FaultClass]++
	}
	// Spread: the seed is useless if all 24 incidents are one device or one
	// class — cosine similarity needs a varied corpus to be interesting.
	if len(seenDevices) < 6 {
		t.Errorf("only %d distinct devices in the corpus, want >= 6", len(seenDevices))
	}
	if len(perClass) < 3 {
		t.Errorf("only %d distinct fault classes, want >= 3", len(perClass))
	}
}

// TestIncidentsMatchLiveFeatureDistribution is the train/serve consistency
// guard: seeded feature vectors must come from the SAME distribution the
// agent writes at run time (simulator physics -> Features -> Normalize),
// not hand-tuned numbers. A hand-tuned corpus would make the incident memory
// look like it works while returning garbage cosine scores.
func TestIncidentsMatchLiveFeatureDistribution(t *testing.T) {
	inc := Incidents(30, time.Now().UTC())
	// temp_mean is index F0TempMean. A freezer corpus must be centered near
	// -18C (healthy) with fault episodes drifting upward — never +20C.
	for _, i := range inc {
		mean := i.Features[ai.F0TempMean]
		if mean > 5 || mean < -35 {
			t.Errorf("%s temp_mean %.1f outside the physical range of the simulator", i.DeviceID, mean)
		}
	}
}

// TestHourlyHistoryShape pins the warm-tier history generator: buckets must
// align to LOCAL hour boundaries (the rollup $dateTruncs with
// Asia/Ho_Chi_Minh), stay strictly in the past (never collide with the
// rollup's live 3h window or its unique index), and carry the same summary
// fields the rollup writes.
func TestHourlyHistoryShape(t *testing.T) {
	now := time.Now().UTC()
	loc := time.FixedZone("ICT", 7*60*60)
	days := 3
	docs := HourlyHistory(FleetDevices, days, now, 42)

	want := len(FleetDevices) * 24 * days
	if len(docs) != want {
		t.Fatalf("got %d docs, want %d (devices x 24 x days)", len(docs), want)
	}
	seen := map[[2]string]bool{}
	nowLocal := now.In(loc)
	for _, d := range docs {
		if d.Unit != "hour" {
			t.Errorf("unit = %s, want hour", d.Unit)
		}
		b := d.Bucket.In(loc)
		if b.Minute() != 0 || b.Second() != 0 {
			t.Errorf("bucket %v not aligned to the local hour", b)
		}
		if !b.Before(time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), nowLocal.Hour(), 0, 0, 0, loc)) {
			t.Errorf("bucket %v is not strictly in the past — collides with the live rollup window", b)
		}
		key := [2]string{d.DeviceID, d.Bucket.UTC().Format(time.RFC3339)}
		if seen[key] {
			t.Errorf("duplicate bucket %v — $merge's unique index would reject it", key)
		}
		seen[key] = true
		if d.MinTempC > d.AvgTempC || d.AvgTempC > d.MaxTempC {
			t.Errorf("summary invariant broken: min %v <= avg %v <= max %v", d.MinTempC, d.AvgTempC, d.MaxTempC)
		}
		if d.Readings <= 0 {
			t.Error("readings must be > 0 (a summary of nothing is not a bucket)")
		}
		if d.CompressorDuty < 0 || d.CompressorDuty > 1 {
			t.Errorf("compressorDuty %v not a fraction", d.CompressorDuty)
		}
	}
}

// TestDailyHistoryShape pins the cold-tier generator the same way, with
// day alignment (17:00Z boundary for Vietnam).
func TestDailyHistoryShape(t *testing.T) {
	now := time.Now().UTC()
	loc := time.FixedZone("ICT", 7*60*60)
	days := 7
	docs := DailyHistory(FleetDevices, days, now, 7)

	if len(docs) != len(FleetDevices)*days {
		t.Fatalf("got %d docs, want %d", len(docs), len(FleetDevices)*days)
	}
	nowLocal := now.In(loc)
	for _, d := range docs {
		b := d.Bucket.In(loc)
		if b.Hour() != 0 || b.Minute() != 0 {
			t.Errorf("bucket %v not aligned to local midnight", b)
		}
		if !b.Before(time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc)) {
			t.Errorf("bucket %v not strictly in the past", b)
		}
		if d.Unit != "day" {
			t.Errorf("unit = %s, want day", d.Unit)
		}
	}
}

func validClass(c string) bool {
	for _, k := range ai.Classes {
		if k == c {
			return true
		}
	}
	return false
}

func validDeviceID(id string) bool {
	for _, d := range FleetDevices {
		if d == id {
			return true
		}
	}
	return false
}
