package ai

import (
	"testing"
	"time"
)

// TestSlopeCPMAccuracy verifies the least-squares fit returns °C/minute.
func TestSlopeCPMAccuracy(t *testing.T) {
	base := time.Now()
	w := NewWindow("S", 60)
	for i := 0; i < 60; i++ {
		// 60 samples at 5s apart = 4.9167 minutes total span.
		// For a +0.3°C/min ramp: per-tick step = 0.3 * (49.167/59) / 60... use
		// exact: total rise 0.3°C/min * 4.9167min = 1.475°C over 59 intervals.
		w.Push(Sample{TS: base.Add(time.Duration(i*5) * time.Second), TempC: -18 + (1.475/59)*float64(i)})
	}
	got := slopeCPM(w.Samples)
	if got < 0.29 || got > 0.31 {
		t.Fatalf("slopeCPM = %f, want ~0.30", got)
	}
}
