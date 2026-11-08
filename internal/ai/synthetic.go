// Package ai — synthetic.go builds labeled feature windows for warm-start
// training. One generator per fault class; the separating signals are:
//   - healthy:            thermostat cycle around setpoint, duty > 0
//   - compressor_failure: warm ramp +0.3 °C/min, duty 0
//   - door_left_open:     duty 1, parked ~2 °C above setpoint, door events
//   - sensor_drift:       cabinet cycles, reported value drifts DOWN
//   - power_outage:       slow warm ramp +0.15 °C/min, duty 0
//
// Scaler (normMean/normStd in normalize.go) is shared by warm-start and
// live inference.
package ai

import (
	"io"
	"log/slog"
	"math"
	"math/rand"
	"time"

	"github.com/onemount/coldtrace/internal/sim"
)

type windowSpec struct {
	class int
	gen   func(i int, r *rand.Rand) Sample
}

func windowSpecs() []windowSpec {
	// Order must match Classifier.Classes:
	// 0 healthy, 1 compressor_failure, 2 door_left_open,
	// 3 sensor_drift, 4 power_outage.
	//
	// Signal design — each class must be separable by the feature set at any
	// window length the agent uses, or the live demo contradicts training.
	// These generators deliberately mirror internal/sim, because that is the
	// physics the classifier is scored against.
	//   0 healthy           thermostat cycle around setpoint; duty > 0
	//   1 compressor_fail   duty 0, fast warm ramp (+0.3 °C/min)
	//   2 door_left_open    duty 1, cabinet parked above setpoint, door events
	//   3 sensor_drift      cabinet still cycles, but reported value travels
	//                       monotonically DOWN (the bias). Direction, not
	//                       shape, is what separates it (F12NetTrendCPM).
	//   4 power_outage      duty 0, slow warm ramp (+0.15 °C/min)
	return []windowSpec{
		{0, func(i int, r *rand.Rand) Sample {
			return Sample{TempC: simCycle(i) + (r.Float64()*2-1)*0.04,
				CompressorRunning: simCycleOn(i), PowerDraw: 400}
		}},
		{1, func(i int, r *rand.Rand) Sample {
			return Sample{TempC: -18 + 0.005*float64(i) + (r.Float64()*2-1)*0.04,
				CompressorRunning: false, PowerDraw: 0}
		}},
		{2, func(i int, r *rand.Rand) Sample {
			return Sample{TempC: -16 + (r.Float64()*2-1)*0.08,
				CompressorRunning: true, PowerDraw: 395, DoorOpen: r.Float64() < 0.35}
		}},
		{3, func(i int, r *rand.Rand) Sample {
			// Residual thermostat shape plus a monotone downward bias.
			return Sample{TempC: simCycle(i) - 0.0025*float64(i) + (r.Float64()*2-1)*0.03,
				CompressorRunning: simCycleOn(i), PowerDraw: 385}
		}},
		{4, func(i int, r *rand.Rand) Sample {
			return Sample{TempC: -18 + 0.0025*float64(i) + (r.Float64()*2-1)*0.04,
				CompressorRunning: false, PowerDraw: 0}
		}},
	}
}

// simCycle / simCycleOn mirror internal/sim's thermostat waveform.
//
// The simulator sits in a 30-tick cycle: ~11 ticks pulling down through the
// band, then a slow coast back to the cut-in point. Measured against a 400-
// seed sweep of the real simulator, this waveform reproduces its steady-state
// signature (mean ≈ -17.85 °C, σ ≈ 0.31, range ≈ 1.1, duty ≈ 0.32) closely
// enough that a model trained on it transfers — which the earlier hand-tuned
// version (σ 0.11, range 0.41, mean -17.65) did not.
func simCycle(i int) float64 {
	c := math.Mod(float64(i), 30)
	if c < 11 {
		return -17.55 - 0.055*c // pull down across the band
	}
	return -18.155 + 0.055*(c-11) // coast back up
}

func simCycleOn(i int) bool { return math.Mod(float64(i), 30) < 11 }

// buildTrainingSet renders every spec into normalized feature vectors.
//
// Each window starts at a random phase of the waveform. Training a single
// aligned phase taught the model a phase rather than a physics signature, so
// it fell apart the moment live windows began mid-cycle; randomising the phase
// (and the duration) is what makes warm-start transfer to the simulator.
func buildTrainingSet(rng *rand.Rand) (xs [][]float64, ys []int) {
	base := time.Now()
	for _, spec := range windowSpecs() {
		for ep := 0; ep < 220; ep++ {
			w := NewWindow("syn", 60)
			phase := rng.Intn(30)
			n := 45 + rng.Intn(30)
			for i := 0; i < n; i++ {
				s := spec.gen(i+phase, rng)
				s.TS = base.Add(time.Duration(i) * time.Second)
				if s.CompressorRunning && s.PowerDraw == 0 {
					s.PowerDraw = 380 + rng.Float64()*40
				}
				w.Push(s)
			}
			xs = append(xs, Normalize(Features(w)))
			ys = append(ys, spec.class)
		}
	}
	return xs, ys
}

// BuildTrainingSetForEval renders nPerClass windows per class using the same
// synthetic generators — exposed for tools/evalmodel (held-out evaluation).
func BuildTrainingSetForEval(rng *rand.Rand, nPerClass int) ([][]float64, []int) {
	base := time.Now()
	var xs [][]float64
	var ys []int
	for _, spec := range windowSpecs() {
		for ep := 0; ep < nPerClass; ep++ {
			w := NewWindow("syn", 60)
			for i := 0; i < 60; i++ {
				s := spec.gen(i, rng)
				s.TS = base.Add(time.Duration(i) * time.Second)
				if s.CompressorRunning && s.PowerDraw == 0 {
					s.PowerDraw = 380 + rng.Float64()*40
				}
				w.Push(s)
			}
			xs = append(xs, Normalize(Features(w)))
			ys = append(ys, spec.class)
		}
	}
	return xs, ys
}

// BuildSimulatorRawSet returns RAW (un-normalised) feature vectors from the
// simulator, one window per class per episode. Exposed so the shared scaler
// can be fitted to the real distribution rather than hand-tuned.
func BuildSimulatorRawSet(nPerClass int) ([][]float64, []int) {
	faults := []string{
		"",
		sim.FaultCompressorFailure,
		sim.FaultDoorLeftOpen,
		sim.FaultSensorDrift,
		sim.FaultPowerOutage,
	}
	var xs [][]float64
	var ys []int
	for class, fault := range faults {
		for ep := 0; ep < nPerClass; ep++ {
			d := sim.NewSeeded("eval", discardLogger(), int64(ep+1)*7919)
			w := NewWindow("eval", 72)
			ts := time.Now().UTC().Add(-time.Duration(ep+2) * time.Hour)
			push := func() {
				r := d.Tick()
				ts = ts.Add(5 * time.Second)
				w.Push(Sample{TS: ts, TempC: r.TempC, PowerDraw: r.PowerDraw,
					CompressorRunning: r.CompressorRunning, DoorOpen: r.DoorOpen})
			}
			for i := 0; i < 120; i++ {
				push()
			}
			if fault != "" {
				d.SetFault(fault)
			}
			for i := 0; i < 72; i++ {
				push()
			}
			xs = append(xs, Features(w))
			ys = append(ys, class)
		}
	}
	return xs, ys
}

// BuildSimulatorEvalSet renders nPerClass windows per class by driving the
// real device simulator (internal/sim) through a warm-up then the fault.
//
// This is deliberately NOT the training distribution: the generators above
// are analytic sketches, while this runs the same thermostat, noise and
// compressor physics the fleet sends over MQTT. A model that scores well here
// will classify the live demo; one that only scores well on its own generators
// will not.
func BuildSimulatorEvalSet(nPerClass int) ([][]float64, []int) {
	faults := []string{
		"", // healthy
		sim.FaultCompressorFailure,
		sim.FaultDoorLeftOpen,
		sim.FaultSensorDrift,
		sim.FaultPowerOutage,
	}
	var xs [][]float64
	var ys []int
	for class, fault := range faults {
		for ep := 0; ep < nPerClass; ep++ {
			d := sim.NewSeeded("eval", discardLogger(), int64(ep+1)*7919)
			w := NewWindow("eval", 72)
			ts := time.Now().UTC().Add(-time.Duration(ep+2) * time.Hour)
			push := func() {
				r := d.Tick()
				ts = ts.Add(5 * time.Second)
				w.Push(Sample{TS: ts, TempC: r.TempC, PowerDraw: r.PowerDraw,
					CompressorRunning: r.CompressorRunning, DoorOpen: r.DoorOpen})
			}
			for i := 0; i < 120; i++ {
				push()
			}
			if fault != "" {
				d.SetFault(fault)
			}
			for i := 0; i < 72; i++ {
				push()
			}
			xs = append(xs, Normalize(Features(w)))
			ys = append(ys, class)
		}
	}
	return xs, ys
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// buildSimulatorTrainingSet renders the warm-start corpus from the device
// simulator: many seeds x phases x window lengths per class, so the model sees
// the run-time distribution rather than a single hand-picked sketch of it.
func buildSimulatorTrainingSet() ([][]float64, []int) {
	faults := []string{
		"",
		sim.FaultCompressorFailure,
		sim.FaultDoorLeftOpen,
		sim.FaultSensorDrift,
		sim.FaultPowerOutage,
	}
	var xs [][]float64
	var ys []int
	for class, fault := range faults {
		for ep := 0; ep < 160; ep++ {
			d := sim.NewSeeded("train", discardLogger(), int64(ep+1)*104729)
			// Vary how much healthy history precedes the fault, and how long
			// the observation window is.
			warm := 40 + (ep%5)*30
			win := 48 + (ep%4)*24
			w := NewWindow("train", win+80)
			ts := time.Now().UTC().Add(-time.Duration(ep+2) * time.Hour)
			push := func() {
				r := d.Tick()
				ts = ts.Add(5 * time.Second)
				w.Push(Sample{TS: ts, TempC: r.TempC, PowerDraw: r.PowerDraw,
					CompressorRunning: r.CompressorRunning, DoorOpen: r.DoorOpen})
			}
			for i := 0; i < warm; i++ {
				push()
			}
			if fault != "" {
				d.SetFault(fault)
			}
			for i := 0; i < win; i++ {
				push()
			}
			// Keep only the observation window.
			if len(w.Samples) > win {
				w.Samples = append([]Sample(nil), w.Samples[len(w.Samples)-win:]...)
			}
			xs = append(xs, Normalize(Features(w)))
			ys = append(ys, class)
		}
	}
	return xs, ys
}
