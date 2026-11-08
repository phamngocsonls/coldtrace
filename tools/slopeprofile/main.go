// Command slopeprofile characterises the temperature slope the dashboard
// computes, by driving the real device simulator (internal/sim).
//
// The status rule flags AT_RISK when a cold cabinet is rising faster than
// atRiskSlopeCPM over a 5-minute window. Choosing that constant needs the
// distribution of slopes a HEALTHY cabinet produces — it is not zero, because
// a thermostat cycle coasts up and then pulls down — and evidence that the
// same rule still fires for each fault, judged at the moment the incident is
// declared rather than at the end of a long ramp.
//
// Run: go run ./tools/slopeprofile
package main

import (
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/onemount/coldtrace/internal/sim"
)

const (
	tick   = 5 * time.Second
	winLen = 61 // 5 minutes of 5 s ticks, matching the $derivative range
	warmup = 120
	settle = 120 // ticks between fault onset and the AT_RISK judgement
	total  = 900
	seeds  = 60
)

var logger = slog.New(slog.NewTextHandler(io.Discard, nil))

// slopeCPM fits a least-squares line through the window and returns °C/min,
// which is what $derivative reports over the same range.
func slopeCPM(buf []float64) float64 {
	n := float64(len(buf))
	var sx, sy, sxy, sxx float64
	for i, t := range buf {
		x := float64(i) * tick.Minutes()
		sx += x
		sy += t
		sxy += x * t
		sxx += x * x
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}

func run(fault string, seed int64, observe func(i int, temp float64, buf []float64)) {
	d := sim.NewSeeded("FZ", logger, seed)
	for i := 0; i < warmup; i++ {
		d.Tick()
	}
	if fault != "" {
		d.SetFault(fault)
	}
	buf := make([]float64, 0, winLen)
	for i := 0; i < total; i++ {
		r := d.Tick()
		buf = append(buf, r.TempC)
		if len(buf) > winLen {
			buf = buf[1:]
		}
		observe(i, r.TempC, buf)
	}
}

func headroom(v []float64) {
	sort.Float64s(v)
	n := len(v)
	pct := func(p float64) float64 { return v[int(p*float64(n-1))] }
	fmt.Printf("  n=%d  p50=%+.3f  p90=%+.3f  p99=%+.3f  p99.9=%+.3f  max=%+.3f\n",
		n, pct(.5), pct(.9), pct(.99), pct(.999), v[n-1])
	for _, t := range []float64{0.05, 0.10, 0.15} {
		c := 0
		for _, x := range v {
			if x > t {
				c++
			}
		}
		fmt.Printf("    > %.2f: %6d  (%.3f%%)\n", t, c, 100*float64(c)/float64(n))
	}
}

func main() {
	// 1. Healthy distribution: every 5-minute window of an hour of rest.
	fmt.Println("healthy — slope of every 5-min window:")
	var healthy []float64
	for seed := int64(1); seed <= seeds; seed++ {
		run("", seed, func(_ int, _ float64, buf []float64) {
			if len(buf) == winLen {
				healthy = append(healthy, slopeCPM(buf))
			}
		})
	}
	headroom(healthy)

	// A fault alarm must be reachable within the demo's patience: check the
	// slope settle ticks after onset, the way the fleet view judges status.
	faults := []string{
		sim.FaultCompressorFailure,
		sim.FaultDoorLeftOpen,
		sim.FaultPowerOutage,
		sim.FaultSensorDrift,
	}
	for _, f := range faults {
		fmt.Printf("\n%s — slope %d s after onset:\n", f, settle*5)
		var at []float64
		for seed := int64(1); seed <= seeds; seed++ {
			run(f, seed, func(i int, _ float64, buf []float64) {
				if i == settle && len(buf) == winLen {
					at = append(at, slopeCPM(buf))
				}
			})
		}
		sort.Float64s(at)
		var trip int
		for _, v := range at {
			if v > 0.10 {
				trip++
			}
		}
		fmt.Printf("  median=%+.3f  p10=%+.3f  trips @0.10: %d/%d\n",
			at[len(at)/2], at[len(at)/10], trip, len(at))
	}

}
