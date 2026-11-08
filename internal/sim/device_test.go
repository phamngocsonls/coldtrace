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

// TestHardFailureBlocksRestartUntilReboot pins the agentic-failure-handling
// physics: compressor_failure_hard is telemetry-identical to
// compressor_failure (same warm ramp — the classifier should call it
// compressor_failure, and only the restart ack reveals it is hard) but a
// remote compressor_restart CANNOT clear it: the controller refuses the
// start (ok:false) and the fault STANDS until a human power-cycles the
// controller. Without this mode the agent's retry->escalate path had no
// real failure to handle — the soft mode always restarts.
func TestHardFailureBlocksRestartUntilReboot(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := NewSeeded("FZ-T02", log, 7)
	d.SetFault(FaultCompressorFailureHard)

	// telemetry physics: same dead-compressor ramp as the soft mode
	start := d.Tick().TempC
	for i := 0; i < 12; i++ { // one minute
		d.Tick()
	}
	if got := d.Tick().TempC; got <= start {
		t.Fatalf("hard failure must warm like a dead compressor: %.2f -> %.2f", start, got)
	}

	// the remote restart is REFUSED and the fault stands
	ack := d.HandleCommand("cmd-refused", ActionCompressorRestart, nil)
	if ack.OK {
		t.Fatal("restart must be refused while the thermal overload is latched")
	}
	if got := d.Faulted(); got != FaultCompressorFailureHard {
		t.Fatalf("fault = %q after refused restart, want it to STAND", got)
	}

	// the human path: power-cycle (reboot) resets the latch
	ack = d.HandleCommand("cmd-reboot", ActionReboot, nil)
	if !ack.OK {
		t.Fatalf("reboot must reset the latch: %+v", ack)
	}
	if got := d.Faulted(); got != FaultNone {
		t.Fatalf("fault = %q after reboot, want cleared", got)
	}
}
