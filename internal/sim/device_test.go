package sim

import (
	"io"
	"log/slog"
	"math"
	"testing"
)

// TestResetRestoresPristineState pins the demo-reset contract: after a fault
// has warmed the cabinet, a reset must return the device to a green state
// immediately (fault cleared, cabinet at setpoint, compressor ready), not just
// stop the fault and leave the fleet cooling for the next hour.
func TestResetRestoresPristineState(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := NewSeeded("FZ-T01", log, 7)

	d.SetFault(FaultCompressorFailure)
	for i := 0; i < 120; i++ { // 10 minutes of a dead compressor
		d.Tick()
	}
	if got := d.Faulted(); got != FaultCompressorFailure {
		t.Fatalf("fault = %q, want %q", got, FaultCompressorFailure)
	}

	d.Reset()
	if got := d.Faulted(); got != FaultNone {
		t.Fatalf("after reset fault = %q, want %q", got, FaultNone)
	}
	r := d.Tick()
	if math.Abs(r.TempC-normalTarget) > 0.2 {
		t.Fatalf("after reset temp = %.2f, want %.2f ±0.2", r.TempC, normalTarget)
	}
	if !r.CompressorRunning || r.DoorOpen || r.PowerDraw == 0 {
		t.Fatalf("after reset reading = %+v, want compressor running, door closed, powered", r)
	}
}
