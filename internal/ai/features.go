// Package ai implements the ColdTrace AI Ops agent: feature extraction,
// online fault classification, failure forecasting, anomaly scoring,
// incident memory and the hybrid autonomy policy engine.
package ai

import (
	"math"
	"time"
)

// Sample is one telemetry reading inside a device window.
type Sample struct {
	TS                time.Time
	TempC             float64
	PowerDraw         float64
	CompressorRunning bool
	DoorOpen          bool
}

// Window is a per-device rolling buffer of recent samples.
type Window struct {
	DeviceID string
	Samples  []Sample // ordered oldest -> newest, capacity-limited
	max      int
}

// NewWindow creates a rolling window holding maxSamples.
func NewWindow(deviceID string, maxSamples int) *Window {
	return &Window{DeviceID: deviceID, max: maxSamples}
}

// Push appends a sample, evicting the oldest beyond capacity.
func (w *Window) Push(s Sample) {
	w.Samples = append(w.Samples, s)
	if len(w.Samples) > w.max {
		// Re-slicing keeps the backing array alive and grows unboundedly with
		// append churn; copy into a right-sized slice instead.
		trimmed := make([]Sample, w.max)
		copy(trimmed, w.Samples[len(w.Samples)-w.max:])
		w.Samples = trimmed
	}
}

// Full reports whether the window has reached its capacity — the length the
// classifier was trained on. A partial window is out-of-distribution: the
// model has never seen one, so its output is confidently wrong (observed
// live: a 28-sample window right after an AI reset classified a healthy
// thermostat coast as compressor_failure at 0.998). Evaluation must wait.
func (w *Window) Full() bool { return len(w.Samples) >= w.max }

// Stale reports whether the window has no recent data.
func (w *Window) Stale(now time.Time, maxAge time.Duration) bool {
	if len(w.Samples) == 0 {
		return true
	}
	return now.Sub(w.Samples[len(w.Samples)-1].TS) > maxAge
}

// Featurizer dimension indices (keep in sync with FeatureNames).
const (
	F0TempMean = iota
	F1TempStd
	F2SlopeCPM
	F3CompressorDuty
	F4PowerMean
	F5DoorEvents
	F6TempRange
	F7SlopeAccel
	F8TempOffset    // mean gap above the window's coldest reading (°C)
	F9DriftLagCPM   // monotone-downward shift rate of the minima (°C/min)
	F10CompressorRe // restart rate: reattains setpoint without decline
	F11TrendLagCPM  // half-window mean shift (°C/min)
	F12NetTrendCPM  // whole-window direction of travel (°C/min)
	NumFeatures
)

// FeatureNames is exported for logs/vector metadata.
var FeatureNames = [NumFeatures]string{
	"temp_mean", "temp_std", "slope_cpm", "compressor_duty",
	"power_mean", "door_events", "temp_range", "slope_accel",
	"temp_offset", "drift_lag_cpm", "compressor_reattain", "trend_lag_cpm",
	"net_trend_cpm",
}

// runningDrawW is the nominal draw of a running compressor, used when the
// payload omits or zeroes its power field.
const runningDrawW = 400.0

// Features extracts the statistical embedding of a device window.
// This vector doubles as the incident "embedding" for incident memory.
func Features(w *Window) []float64 {
	n := len(w.Samples)
	f := make([]float64, NumFeatures)
	if n < 4 {
		return f
	}

	// mean / std
	sum, sumSq := 0.0, 0.0
	for _, s := range w.Samples {
		sum += s.TempC
		sumSq += s.TempC * s.TempC
	}
	mean := sum / float64(n)
	variance := sumSq/float64(n) - mean*mean
	f[F0TempMean] = mean
	f[F1TempStd] = math.Sqrt(math.Max(0, variance))

	// least-squares slope in °C/minute over the window
	// Slope of the *reported* temperature: sensor drift is a reporting fault
	// (the physical cabinet stays cold), so the trend must be taken after the
	// sensor offset is applied, exactly as the device transmits it.
	f[F2SlopeCPM] = slopeCPM(w.Samples)

	// Compressor duty cycle.
	//
	// Power is derived from the compressor's own run flag rather than the
	// payload's draw field: a running compressor draws current by definition,
	// so a missing/zero `powerDraw` in a reading must not be read as an
	// outage. A true power outage is *already* captured by duty == 0, which is
	// the physically meaningful signal — inferring it a second time from a
	// field the device may simply not report makes the two classes
	// indistinguishable at inference time.
	run := 0
	for _, s := range w.Samples {
		if s.CompressorRunning {
			run++
		}
	}
	f[F3CompressorDuty] = float64(run) / float64(n)
	if run > 0 {
		f[F4PowerMean] = runningDrawW
	}

	// door events
	doors := 0
	for _, s := range w.Samples {
		if s.DoorOpen {
			doors++
		}
	}
	f[F5DoorEvents] = float64(doors)

	// range
	minT, maxT := w.Samples[0].TempC, w.Samples[0].TempC
	for _, s := range w.Samples {
		minT = math.Min(minT, s.TempC)
		maxT = math.Max(maxT, s.TempC)
	}
	f[F6TempRange] = maxT - minT

	// slope acceleration: slope of first half minus second half
	if n >= 8 {
		h := n / 2
		f[F7SlopeAccel] = slopeCPM(w.Samples[h:]) - slopeCPM(w.Samples[:h])
	}

	// Sensor offset: gap between the reported temperature and the cabinet
	// the compressor telemetry implies. A reporting fault leaves the cabinet
	// cold while the sensor drifts, so the offset grows even though the
	// cabinet is healthy — this is the only feature that separates
	// sensor_drift from healthy, whose slopes both hover near zero.
	f[F8TempOffset] = sensorOffsetC(w.Samples)
	f[F9DriftLagCPM] = driftLagCPM(w.Samples)
	f[F10CompressorRe] = compressorReattain(w.Samples)
	f[F11TrendLagCPM] = trendLagCPM(w.Samples)
	f[F12NetTrendCPM] = netTrendCPM(w.Samples)
	return f
}

// sensorOffsetC is the mean gap between reported readings and the window's
// coldest reading, in °C. It is a coarse "how far has the cabinet been
// allowed to sit above its low point" measure and is near zero for a healthy,
// tight cabinet.
func sensorOffsetC(samples []Sample) float64 {
	clean := dedupeByTS(samples)
	if len(clean) < 4 {
		return 0
	}
	coldest := clean[0].TempC
	for _, s := range clean {
		if s.TempC < coldest {
			coldest = s.TempC
		}
	}
	var sum float64
	for _, s := range clean {
		sum += s.TempC - coldest
	}
	return sum / float64(len(clean))
}

// driftLagCPM measures whether the rolling minima walk *monotonically downward*
// — the fingerprint of a slow sensor bias — as distinct from a cabinet that
// merely oscillates around setpoint.
//
// It takes the coldest reading of each half of the window and returns their
// difference divided by the first half's duration (a "per-half-window" rate —
// consistent across callers and training, not a literal °C/minute of elapsed
// time). A healthy cabinet oscillates around one
// setpoint and lands near zero (either sign). An under-reporting sensor attains
// a new low only in the second half, so the result is negative and grows with
// window length. Measured on the reference fleet: healthy -0.007..|+0.007|,
// sensor_drift -0.014..-0.007 at a 6-minute window — a sign-flipped range no
// other class occupies (every warming fault is strictly positive).
func driftLagCPM(samples []Sample) float64 {
	return halfMinDeltaCPM(samples, func(a, b []Sample) (float64, float64) {
		return minTemp(a), minTemp(b)
	})
}

// driftLagMetricCPM is the *magnitude* of the half-window minima shift, used
// as a feature so the linear model sees a non-negative, direction-free signal.
func driftLagMetricCPM(samples []Sample) float64 {
	return -driftLagCPM(samples)
}

// trendLagCPM is the mean-based counterpart of driftLagCPM, used for the
// warming classes where the whole distribution shifts.
func trendLagCPM(samples []Sample) float64 {
	return halfMinDeltaCPM(samples, func(a, b []Sample) (float64, float64) {
		return meanTemp(a), meanTemp(b)
	})
}

// netTrendCPM is the mean slope of the whole window in °C/min — the direction
// the cabinet is actually travelling. A healthy thermostat oscillates around
// setpoint, so its net trend takes either sign; a drifting sensor only ever
// moves one way, which is the signature that separates them.
func netTrendCPM(samples []Sample) float64 {
	clean := dedupeByTS(samples)
	if len(clean) < 8 {
		return 0
	}
	spanMin := clean[len(clean)-1].TS.Sub(clean[0].TS).Minutes()
	if spanMin <= 0 {
		return 0
	}
	return (meanTemp(clean[len(clean)/2:]) - meanTemp(clean[:len(clean)/2])) / (spanMin / 2)
}

func halfMinDeltaCPM(samples []Sample, f func(a, b []Sample) (float64, float64)) float64 {
	clean := dedupeByTS(samples)
	n := len(clean)
	if n < 8 {
		return 0
	}
	h := n / 2
	first, second := f(clean[:h], clean[h:])
	// Normalise by the *half-window* span, not the full window: each estimate
	// represents its own half, so dividing by the whole duration understates
	// the rate by 2x and makes thresholds depend on how long the window is.
	spanMin := clean[h-1].TS.Sub(clean[0].TS).Minutes()
	if spanMin <= 0 {
		return 0
	}
	return (second - first) / spanMin
}

func minTemp(ss []Sample) float64 {
	m := ss[0].TempC
	for _, s := range ss {
		if s.TempC < m {
			m = s.TempC
		}
	}
	return m
}

func meanTemp(ss []Sample) float64 {
	var sum float64
	for _, s := range ss {
		sum += s.TempC
	}
	return sum / float64(len(ss))
}

// compressorReattain is the fraction of compressor-on samples that sit within
// 1°C of the window low: a healthy compressor keeps clawing the cabinet back
// to setpoint. A door_left_open compressor runs continuously yet never gets
// there, so this stays low while duty is high — separating it from healthy.
func compressorReattain(samples []Sample) float64 {
	clean := dedupeByTS(samples)
	if len(clean) < 4 {
		return 0
	}
	coldest := clean[0].TempC
	for _, s := range clean {
		if s.TempC < coldest {
			coldest = s.TempC
		}
	}
	on, back := 0, 0
	for _, s := range clean {
		if s.CompressorRunning {
			on++
		}
		if s.TempC-coldest <= 1.0 {
			back++
		}
	}
	// "Does the cabinet sit near its coldest reading?" — measured over the
	// whole window, not just compressor-on samples. A dead compressor that
	// starts from cold would otherwise score 0.0 (looks alarming) and a
	// powered-off cabinet would score 0.5, while both are really "the box is
	// fine and nothing is running". Warm boxes score low either way, which is
	// what door_left_open needs.
	_ = on
	return float64(back) / float64(len(clean))
}

// slopeCPM fits temp = a + b*t (minutes) and returns b.
//
// Samples sharing a timestamp are collapsed first: the live ingest path can
// hold several readings with an identical TS (a device republishing a batch,
// or a simulator emitting faster than the clock resolution it is sampled at).
// A zero-length time axis turns the least-squares denominator into noise,
// producing slopes in the millions of °C/min that saturate every downstream
// z-score. Treating coincident samples as one observation is both physically
// right (no time passed, no trend) and numerically stable.
func slopeCPM(samples []Sample) float64 {
	clean := dedupeByTS(samples)
	n := float64(len(clean))
	if n < 3 {
		return 0
	}
	t0 := clean[0].TS
	var st, stt, sy, sty float64
	for _, s := range clean {
		t := s.TS.Sub(t0).Minutes()
		st += t
		stt += t * t
		sy += s.TempC
		sty += t * s.TempC
	}
	denom := n*stt - st*st
	if denom <= 1e-9 {
		return 0
	}
	return (n*sty - st*sy) / denom
}

// dedupeByTS collapses runs of identical timestamps, keeping the freshest
// value written at that instant.
//
// PRECONDITION: samples must be sorted by TS ascending (the ingest path sorts
// by ts). It collapses only *adjacent* duplicates, so an unsorted slice would
// leave non-adjacent repeats in place and produce a nonsense slope. Callers
// outside the ingest path must sort first.
func dedupeByTS(samples []Sample) []Sample {
	if len(samples) < 2 {
		return samples
	}
	out := make([]Sample, 0, len(samples))
	for _, s := range samples {
		if len(out) > 0 && out[len(out)-1].TS.Equal(s.TS) {
			out[len(out)-1] = s
			continue
		}
		out = append(out, s)
	}
	return out
}

// Forecast projects the temperature `horizonMin` ahead using an
// EWMA-smoothed slope and returns minutes until the alarm threshold
// (-10°C) is crossed (math.NaN() if not on a rising trajectory).
func Forecast(w *Window, horizonMin float64, alarmC float64) (projectedC, etaAlarmMin float64) {
	// Gate on the DEDUPED length: the projection is computed from distinct
	// instants, so gating on raw sample count would refuse to forecast a
	// warming cabinet that happens to have republished its timestamps. That
	// silence is worse than a noisy forecast — EtaGate reads NaN as "no
	// trend", so an operator would never be warned.
	clean := dedupeByTS(w.Samples)
	if len(clean) < 6 {
		return math.NaN(), math.NaN()
	}
	last := clean[len(clean)-1].TempC
	// EWMA over trailing slopes (alpha 0.4): one noisy last interval must not
	// swing a projection an operator will act on.
	const alpha = 0.4
	smooth := slopeCPM(clean)
	for i := 3; i <= len(clean); i++ {
		smooth = alpha*slopeCPM(clean[:i]) + (1-alpha)*smooth
	}
	projectedC = last + smooth*horizonMin
	if smooth <= 0.01 {
		return projectedC, math.NaN()
	}
	etaAlarmMin = (alarmC - last) / smooth
	return projectedC, etaAlarmMin
}

// AnomalyScorer tracks an EWMA baseline of temperature per device and
// scores deviations as 0..1.
type AnomalyScorer struct {
	mean, variance float64
	count          float64
	alpha          float64
}

// NewAnomalyScorer seeds the EWMA at the setpoint.
func NewAnomalyScorer() *AnomalyScorer {
	return &AnomalyScorer{mean: -18.0, variance: 0.25, alpha: 0.08}
}

// Score ingests a reading and returns anomaly score in [0,1].
func (a *AnomalyScorer) Score(tempC float64) float64 {
	a.count++
	z := (tempC - a.mean) / math.Sqrt(math.Max(a.variance, 1e-6))
	// blend EWMA toward the new observation (slower on extreme values)
	w := a.alpha
	if z > 3 {
		w = a.alpha * 0.25 // resist absorbing shocks into the baseline
	}
	a.mean += w * (tempC - a.mean)
	d := (tempC - a.mean) * (tempC - a.mean)
	a.variance += w * (d - a.variance)
	return math.Min(1, math.Max(0, z/6)) // z=6 -> score 1
}

// Similarity returns cosine similarity between two equal-length vectors.
func Similarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// TopSimilar and SimilarMatch live in similar.go; the search itself is
// IncidentMemory.TopSimilar in memory.go.
