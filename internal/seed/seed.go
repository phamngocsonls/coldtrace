// Package seed generates demo HISTORY that is consistent with the live
// pipeline — the difference between a demo that shows its workings and
// one that shows props.
//
// Two gaps this exists to close (both found by the pre-demo audit):
//
//  1. The AI agent's incident memory (ai.IncidentMemory) scans RESOLVED
//     incidents. A fresh stack has none, so the "similar past incidents"
//     panel renders empty on stage — a feature that looks broken exactly
//     when it is meant to shine. Seeded incidents fix the cold start.
//
//  2. The warm/cold tiers (telemetry_hourly/daily) only accumulate from
//     stack start-up, so the tier chart shows a few hours. Seeded history
//     makes the retention story visible: 7 days of hourly, a year of
//     daily — all older than the rollup's live window, so nothing collides.
//
// The incident feature vectors come from ai.BuildSimulatorEvalSet — the
// same simulator physics, the same Features/Normalize the agent runs at
// serve time. Hand-tuned vectors would make cosine similarity return
// garbage while looking like it works; this way the incident memory is
// exercised on the exact distribution it will see live (train/serve
// consistency, the same decision the classifier warm-start made).
//
// Summary documents match the rollup's $project shape exactly (cmd/rollup),
// bucketed to LOCAL hour/day boundaries the same way its $dateTrunc cuts
// them — a summary row whose bucket does not align would be obviously fake
// the moment anyone zoomed in.
//
// usage (see tools/seedhistory):
//
//	go run ./tools/seedhistory            # 24 incidents + 7d hourly + 21d daily
package seed

import (
	"math"
	"math/rand"
	"time"

	"github.com/onemount/coldtrace/internal/ai"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// FleetDevices is the 12-freezer fleet from tools/initdb.
var FleetDevices = []string{
	"FZ-001", "FZ-002", "FZ-003", "FZ-004", "FZ-005", "FZ-006",
	"FZ-007", "FZ-008", "FZ-009", "FZ-010", "FZ-011", "FZ-012",
}

// Incident is the subset of the ai_incidents document the incident memory and
// the console read: identity, class, confidence, decision, the feature
// vector, and the episode timestamps. It mirrors the field names the agent
// writes (cmd/ai-agent incidentDoc) — a mismatched bson tag here would
// silently produce incidents that never match a cosine query.
type Incident struct {
	DeviceID     string    `bson:"deviceId"    json:"deviceId"`
	FaultClass   string    `bson:"faultClass" json:"faultClass"`
	Confidence   float64   `bson:"confidence" json:"confidence"`
	Features     []float64 `bson:"features"    json:"features"`
	Decision     string    `bson:"decision"    json:"decision"`
	Reason       string    `bson:"reason"      json:"reason"`
	Status       string    `bson:"status"      json:"status"`
	RemediatedAt time.Time `bson:"remediatedAt" json:"remediatedAt"`
	RemediatedBy string    `bson:"remediatedBy" json:"remediatedBy"`
	OpenedAt     time.Time `bson:"openedAt"    json:"openedAt"`
	Acknowledged *bool     `bson:"acknowledged" json:"acknowledged"`
	Seeded       bool      `bson:"seeded"      json:"seeded"`
}

// Summary is one tier document — field-for-field the rollup's $project
// (cmd/rollup runRollup), so the seeded rows and the live ones are
// indistinguishable to the reader (internal/aggdb.TierHistory).
type Summary struct {
	DeviceID       string    `bson:"deviceId"       json:"deviceId"`
	Bucket         time.Time `bson:"bucket"         json:"bucket"`
	Unit           string    `bson:"unit"           json:"unit"`
	AvgTempC       float64   `bson:"avgTempC"       json:"avgTempC"`
	MinTempC       float64   `bson:"minTempC"       json:"minTempC"`
	MaxTempC       float64   `bson:"maxTempC"       json:"maxTempC"`
	AvgPowerDraw   float64   `bson:"avgPowerDraw"   json:"avgPowerDraw"`
	CompressorDuty float64   `bson:"compressorDuty" json:"compressorDuty"`
	DoorEvents     int64     `bson:"doorEvents"     json:"doorEvents"`
	Readings       int64     `bson:"readings"       json:"readings"`
	// Seeded marks generator output so re-seeding replaces exactly this
	// set — never a bucket the rollup $merge'd live. The tier reader's
	// explicit $project never surfaces it.
	Seeded bool `bson:"seeded" json:"-"`
}

// reasons is the remediation narrative per class — the same vocabulary the
// agent's policy engine emits, so a seeded timeline reads like a real one.
var reasons = map[string]string{
	"compressor_failure": "compressor duty pinned at 1.0 with no pull-down; restart restored setpoint within 8 min",
	"door_left_open":     "sustained door-open signal with rising temp; defrost cycle + door alarm cleared it",
	"sensor_drift":       "readings diverged from twin's thermal model; sensor recalibration restored agreement",
	"power_outage":       "power draw fell to zero while temp rose; supply restored, pull-down confirmed",
}

// Incidents builds n RESOLVED historical incidents with REAL feature
// vectors: ai.BuildSimulatorEvalSet runs the actual device simulator
// through warm-up -> fault windows and extracts Normalize(Features(w)) —
// the same numbers the agent stores at serve time.
func Incidents(n int, now time.Time) []Incident {
	if n <= 0 {
		return nil
	}
	// n/4 episodes per fault class (4 classes carry incidents; healthy
	// windows are filtered out — there is no "healthy" incident).
	nPerClass := (n + 3) / 4
	xs, ys := ai.BuildSimulatorEvalSet(nPerClass)

	// Confidence comes from the classifier itself, so the seeded value
	// is one a live prediction could actually produce.
	cl := ai.NewClassifier(nil)

	out := make([]Incident, 0, n)
	for i := 0; i < len(xs) && len(out) < n; i++ {
		classIdx := ys[i]
		if classIdx == 0 {
			continue // healthy windows are not incidents
		}
		class := ai.Classes[classIdx]
		dev := FleetDevices[len(out)%len(FleetDevices)]

		// Spread episodes across the past 6 days, deterministic per index.
		hoursAgo := 3 + (len(out)*7)%(6*24)
		opened := now.Add(-time.Duration(hoursAgo) * time.Hour)
		ack := true
		conf := 0.92
		if _, probs := cl.Predict(xs[i]); probs != nil {
			if p, ok := probs[class]; ok && p > 0.5 {
				conf = round3(p)
			}
		}
		out = append(out, Incident{
			DeviceID:     dev,
			FaultClass:   class,
			Confidence:   conf,
			Features:     xs[i],
			Decision:     "AUTO",
			Reason:       reasons[class],
			Status:       "RESOLVED",
			RemediatedAt: opened.Add(time.Duration(4+len(out)%7) * time.Minute),
			RemediatedBy: "ai-agent",
			OpenedAt:     opened,
			Acknowledged: &ack,
			Seeded:       true,
		})
	}
	return out
}

// HourlyHistory generates per-device hourly summaries for the past `days`
// LOCAL days, strictly before the current local hour — never inside the
// rollup's live 3h window, so the $merge unique index is safe.
func HourlyHistory(devices []string, days int, now time.Time, rngSeed int64) []Summary {
	loc := time.FixedZone("ICT", 7*60*60)
	nowLocal := now.In(loc)
	// current local hour start, then back off; hour 0 of the loop is the
	// newest bucket, which must still be at least one hour old.
	cur := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), nowLocal.Hour(), 0, 0, 0, loc)
	rng := rand.New(rand.NewSource(rngSeed))

	out := make([]Summary, 0, len(devices)*days*24)
	for _, dev := range devices {
		// Per-device base so traces differ visibly between freezers.
		base := -18 + rng.Float64()*0.8 - 0.4
		for h := days * 24; h >= 1; h-- {
			bucket := cur.Add(-time.Duration(h) * time.Hour).UTC()
			// Diurnal wave + noise: cabinets work harder in the afternoon.
			hourOfDay := bucket.In(loc).Hour()
			wave := math.Sin(float64(hourOfDay)/24*2*math.Pi) * 0.6
			avg := base + wave + (rng.Float64()-0.5)*0.3
			spread := 0.2 + rng.Float64()*0.4
			out = append(out, Summary{
				DeviceID:       dev,
				Bucket:         bucket,
				Unit:           "hour",
				AvgTempC:       round2(avg),
				MinTempC:       round2(avg - spread),
				MaxTempC:       round2(avg + spread),
				AvgPowerDraw:   round2(395 + rng.Float64()*35 + wave*20),
				CompressorDuty: round2(0.34 + 0.12*math.Max(0, wave) + rng.Float64()*0.08),
				DoorEvents:     int64(rng.Intn(4)),
				Readings:       720, // 5s cadence x 3600s
				Seeded:         true,
			})
		}
	}
	return out
}

// DailyHistory generates per-device daily summaries for the past `days`
// LOCAL days, strictly before today's local midnight.
func DailyHistory(devices []string, days int, now time.Time, rngSeed int64) []Summary {
	loc := time.FixedZone("ICT", 7*60*60)
	nowLocal := now.In(loc)
	today := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc)
	rng := rand.New(rand.NewSource(rngSeed + 1))

	out := make([]Summary, 0, len(devices)*days)
	for _, dev := range devices {
		base := -18 + rng.Float64()*0.6 - 0.3
		for d := days; d >= 1; d-- {
			bucket := today.Add(-time.Duration(d) * 24 * time.Hour).UTC()
			avg := base + (rng.Float64()-0.5)*0.25
			spread := 0.15 + rng.Float64()*0.3
			out = append(out, Summary{
				DeviceID:       dev,
				Bucket:         bucket,
				Unit:           "day",
				AvgTempC:       round2(avg),
				MinTempC:       round2(avg - spread),
				MaxTempC:       round2(avg + spread),
				AvgPowerDraw:   round2(398 + rng.Float64()*22),
				CompressorDuty: round2(0.4 + rng.Float64()*0.08),
				DoorEvents:     int64(18 + rng.Intn(22)),
				Readings:       17280, // 5s x 86400s
				Seeded:         true,
			})
		}
	}
	return out
}

// IncidentDocs / SummaryDocs adapt the typed slices for InsertMany.
func IncidentDocs(inc []Incident) []any {
	out := make([]any, len(inc))
	for i := range inc {
		out[i] = inc[i]
	}
	return out
}

func SummaryDocs(s []Summary) []any {
	out := make([]any, len(s))
	for i := range s {
		out[i] = s[i]
	}
	return out
}

// ReplaceFilter identifies seeded incidents for idempotent re-seeding.
func ReplaceFilter() bson.M { return bson.M{"seeded": true} }

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
