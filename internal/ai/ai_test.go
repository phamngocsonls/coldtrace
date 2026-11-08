package ai

import (
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/onemount/coldtrace/internal/sim"
)

// TestClassifierSeparation proves the warm-started model separates the
// fault classes on windows produced by the SAME physics the fleet simulator
// runs (internal/sim), not by re-running the training generators. That is the
// only version of this test that means anything: training-set accuracy is
// circular by construction and is covered separately by tools/evalmodel.
//
// Each case warms a real simulator device up, injects the fault, and lets it
// run. Timestamps are synthetic at the fleet's 5 s cadence; the simulator's
// physics is the same, and the classifier never sees wall-clock time.
func TestClassifierSeparation(t *testing.T) {
	c := NewClassifier(testLogger())

	type tc struct {
		fault string
		want  string
	}
	cases := []tc{
		{"", ClassHealthy},
		{sim.FaultCompressorFailure, ClassCompressorFailure},
		{sim.FaultDoorLeftOpen, ClassDoorLeftOpen},
		{sim.FaultSensorDrift, ClassSensorDrift},
		{sim.FaultPowerOutage, ClassPowerOutage},
	}
	for _, c2 := range cases {
		t.Run(func() string {
			if c2.fault == "" {
				return "healthy"
			}
			return c2.fault
		}(), func(t *testing.T) {
			// Fixed seed: the simulator is stochastic by design, but a flaky
			// classifier test proves nothing.
			d := sim.NewSeeded("FZ-001", slog.New(slog.NewTextHandler(io.Discard, nil)), 7)
			w := NewWindow("FZ-001", 72) // 6 min at 5 s — the agent's window
			ts := time.Now().UTC().Add(-2 * time.Hour)
			push := func() {
				r := d.Tick()
				ts = ts.Add(5 * time.Second)
				w.Push(Sample{TS: ts, TempC: r.TempC, PowerDraw: r.PowerDraw,
					CompressorRunning: r.CompressorRunning, DoorOpen: r.DoorOpen})
			}
			for i := 0; i < 80; i++ {
				push() // settle into a stable cycle
			}
			if c2.fault != "" {
				d.SetFault(c2.fault)
			}
			for i := 0; i < 72; i++ {
				push()
			}
			got, probs := c.Predict(Normalize(Features(w)))
			if got != c2.want {
				t.Errorf("got %s (p=%.3f, probs=%v), want %s", got, probs[got], probs, c2.want)
			}
			if probs[got] < 0.6 {
				t.Errorf("confidence %.2f too low for %s", probs[got], c2.want)
			}
		})
	}
}

// TestDegenerateTimestamps is a regression test for the bug that made every
// live prediction `compressor_failure` at p=1.0: when several readings share
// one timestamp the least-squares denominator collapses and the slope
// explodes to ~1e6 °C/min, saturating every downstream z-score.
func TestDegenerateTimestamps(t *testing.T) {
	now := time.Now().UTC()
	// A dead thermal cabinet (constant temperature) whose readings collapse
	// onto few timestamps. Only the slope claim is asserted here: a *constant*
	// temperature is genuinely ambiguous between a healthy box and a dead one
	// with no compressor data, and no classifier should be required to resolve
	// that. What must never happen is the slope exploding.
	w := NewWindow("T", 60)
	for i := 0; i < 60; i++ {
		w.Push(Sample{TS: now.Add(time.Duration(i/20) * time.Second), TempC: -18})
	}
	f := Features(w)
	if math.Abs(f[F2SlopeCPM]) > 1.0 {
		t.Errorf("slope from coincident timestamps = %v °C/min, want ~0", f[F2SlopeCPM])
	}
	// And the realistic healthy case — a cycling compressor — must classify.
	h := NewWindow("T", 72)
	base := now.Add(-6 * time.Minute)
	for i := 0; i < 72; i++ {
		cycle := i % 20
		tc := -18.0
		if cycle >= 14 {
			tc = -17.97
		}
		h.Push(Sample{TS: base.Add(time.Duration(i) * 5 * time.Second), TempC: tc,
			CompressorRunning: cycle < 14, PowerDraw: 400})
	}
	if got, p := NewClassifier(testLogger()).Predict(Normalize(Features(h))); got != ClassHealthy {
		t.Errorf("cycling healthy cabinet classified as %s (p=%.2f), want healthy", got, p[got])
	}
}

// TestForecastTrends guards the EWMA forecast: a consistent warm ramp must
// produce a finite, sensible ETA; a flat cabinet must produce none.
func TestForecastTrends(t *testing.T) {
	now := time.Now().UTC()
	ramp := NewWindow("T", 60)
	for i := 0; i < 60; i++ {
		ramp.Push(Sample{TS: now.Add(time.Duration(i) * 5 * time.Second), TempC: -18 + 0.05*float64(i)})
	}
	proj, eta := Forecast(ramp, 10, -10)
	if math.IsNaN(eta) || eta <= 0 || eta > 120 {
		t.Errorf("ramp forecast eta = %v min (proj %v), want a finite value under 2h", eta, proj)
	}

	flat := NewWindow("T", 60)
	for i := 0; i < 60; i++ {
		flat.Push(Sample{TS: now.Add(time.Duration(i) * 5 * time.Second), TempC: -18})
	}
	if _, eta := Forecast(flat, 10, -10); !math.IsNaN(eta) {
		t.Errorf("flat cabinet produced an alarm ETA of %v min, want NaN", eta)
	}
}

// TestPolicyHybrid covers propose -> approve -> auto with guardrails.
func TestPolicyHybrid(t *testing.T) {
	cfg := DefaultPolicy()
	now := time.Now()
	input := PolicyInput{
		DeviceID: "FZ-003", FaultClass: ClassCompressorFailure,
		Confidence: 0.95, EtaAlarmMin: 5, AlarmingNow: 0,
		LastAutoFor: map[string]time.Time{}, Now: now,
	}

	// 1) unapproved -> PROPOSED
	r := Evaluate(input, cfg, map[string]bool{})
	if r.Decision != DecisionPropose {
		t.Fatalf("want PROPOSED, got %s (%s)", r.Decision, r.Reason)
	}

	// 2) approved + gates pass -> AUTO
	r = Evaluate(input, cfg, map[string]bool{ClassCompressorFailure: true})
	if r.Decision != DecisionAuto {
		t.Fatalf("want AUTO, got %s (%s)", r.Decision, r.Reason)
	}

	// 3) fleet circuit breaker -> HOLD
	input2 := input
	input2.AlarmingNow = 2
	r = Evaluate(input2, cfg, map[string]bool{ClassCompressorFailure: true})
	if r.Decision != DecisionHold || !r.Circuit {
		t.Fatalf("want HOLD(circuit), got %s circuit=%v", r.Decision, r.Circuit)
	}

	// 4) cooldown -> HOLD
	input3 := input
	input3.LastAutoFor = map[string]time.Time{"FZ-003": now.Add(-5 * time.Minute)}
	r = Evaluate(input3, cfg, map[string]bool{ClassCompressorFailure: true})
	if r.Decision != DecisionHold || !r.Cooldown {
		t.Fatalf("want HOLD(cooldown), got %s cooldown=%v", r.Decision, r.Cooldown)
	}

	// 5) low confidence -> HOLD
	input4 := input
	input4.Confidence = 0.6
	r = Evaluate(input4, cfg, map[string]bool{ClassCompressorFailure: true})
	if r.Decision != DecisionHold {
		t.Fatalf("want HOLD(conf), got %s (%s)", r.Decision, r.Reason)
	}
}

// TestSimilarity sanity-checks the cosine ranking used by incident memory.
func TestSimilarity(t *testing.T) {
	a := []float64{1, 0, 0}
	b := []float64{1, 0, 0}
	c := []float64{0, 1, 0}
	if Similarity(a, b) < 0.99 || Similarity(a, c) > 0.01 {
		t.Fatal("cosine similarity broken")
	}
}

// TestAnomalyScorer rising temps must push score up.
func TestAnomalyScorer(t *testing.T) {
	s := NewAnomalyScorer()
	for i := 0; i < 100; i++ {
		s.Score(-18)
	}
	score := s.Score(-12)
	if score < 0.5 {
		t.Fatalf("expected high anomaly score for +6C shock, got %.2f", score)
	}
}
