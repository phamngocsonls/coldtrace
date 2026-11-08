package aggdb

import "testing"

// The dashboard flags a device AT_RISK when its slope exceeds atRiskSlopeCPM.
// That threshold is only meaningful if the slope averages over at least one
// thermostat cycle: a freezer pulls down quickly and then coasts for minutes,
// so a window shorter than a cycle reports whichever half it straddles — a
// perfectly healthy cabinet coasting reads a rising ~+0.1 °C/min and trips the
// alarm on its own normal behaviour.
//
// This test pins the window to 5 minutes. It is a constraint on the pipeline
// definition rather than on arithmetic, but it is the kind of detail that gets
// shortened during "cleanup" and silently breaks the fleet view.
func TestSlopeWindowCoversThermostatCycle(t *testing.T) {
	stage := slopeStage()

	var setWindow interface{}
	for _, f := range stage {
		if f.Key == "$setWindowFields" {
			setWindow = f.Value
		}
	}
	if setWindow == nil {
		t.Fatal("slopeStage has no $setWindowFields stage")
	}

	if got := slopeWindowMinutes(t, setWindow); got < 5 {
		t.Fatalf("slope window is %v minutes; need >= 5 so a thermostat cycle averages out", got)
	}
}

// TestStatusThresholdsMatchDocs keeps the ladder in aggdb in step with the
// documented rule (ALARM above -10, AT_RISK above -14 or rising).
func TestStatusThresholdsMatchDocs(t *testing.T) {
	var branches interface{}
	for _, f := range statusSwitch() {
		if f.Key == "$switch" {
			branches = f.Value
		}
	}
	if branches == nil {
		t.Fatal("statusSwitch has no $switch")
	}

	src := sprint(branches)
	for _, want := range []string{"ALARM", "AT_RISK"} {
		if !containsStr(src, want) {
			t.Errorf("status switch is missing the %s branch", want)
		}
	}
	for _, want := range []string{"-10", "-14"} {
		if !containsStr(src, want) {
			t.Errorf("status switch is missing threshold %s", want)
		}
	}
	if !containsStr(src, sprint(atRiskSlopeCPM)) {
		t.Errorf("status switch does not use atRiskSlopeCPM")
	}
}

// TestAtRiskSlopeClearsHealthyNoise pins the slope threshold between the two
// distributions it has to separate. Both numbers come from
// `go run ./tools/slopeprofile`, which drives the real device simulator:
//
//   - healthy, ~50k five-minute windows: p99.9 = +0.090, max = +0.105 °C/min
//   - compressor_failure and power_outage, 10 min after onset: +0.30 and
//     +0.15 °C/min
//
// A threshold at or below the healthy tail flickers a resting fleet yellow (the
// original +0.05 tripped 1.5% of healthy samples); one above the fault floor
// misses the faults the demo is built to catch.
func TestAtRiskSlopeClearsHealthyNoise(t *testing.T) {
	const (
		healthyP999CPM = 0.090 // healthy p99.9 — about one sample in a thousand
		faultFloorCPM  = 0.150 // power_outage, the slowest fault that ramps
	)

	if atRiskSlopeCPM <= healthyP999CPM {
		t.Errorf("atRiskSlopeCPM = %.3f is at or below the healthy p99.9 %.3f — resting freezers will flicker AT_RISK",
			atRiskSlopeCPM, healthyP999CPM)
	}
	if atRiskSlopeCPM >= faultFloorCPM {
		t.Errorf("atRiskSlopeCPM = %.3f is at or above the slowest fault ramp %.3f — real faults will not trip AT_RISK",
			atRiskSlopeCPM, faultFloorCPM)
	}
}
