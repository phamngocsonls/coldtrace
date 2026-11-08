// Package sim implements the virtual freezer device physics:
// normal operation around -18°C plus fault modes the AI classifier knows:
//   - compressor_failure: dead compressor, cabinet ramps up ~+0.3°C/minute
//   - door_left_open: door wedged open, warm ingress ~+0.1°C/minute
//   - sensor_drift: the temperature sensor slowly under-reports (reads low)
//   - power_outage: no compressor power at all
package sim

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"sync"
	"time"
)

// Fault modes (mirrors the classifier classes; "healthy" = none).
const (
	FaultNone              = ""
	FaultCompressorFailure = "compressor_failure"
	FaultDoorLeftOpen      = "door_left_open"
	FaultSensorDrift       = "sensor_drift"
	FaultPowerOutage       = "power_outage"
)

// Action is a control command the device understands.
type Action string

const (
	ActionCompressorRestart      Action = "compressor_restart"
	ActionEnterCompressorFailure Action = "enter_compressor_failure"
	ActionSetFault               Action = "set_fault"    // payload: {"fault":"door_left_open"}
	ActionSetSetpoint            Action = "set_setpoint" // payload: {"setpoint":-20}
	ActionDefrost                Action = "defrost"
	ActionReboot                 Action = "reboot"
	// ActionDemoReset restores a pristine demo state (fault-free at the
	// default setpoint). Distinct from compressor_restart, which lets the
	// cabinet pull down gradually so the recovery arc stays visible.
	ActionDemoReset Action = "demo_reset"
)

const (
	normalTarget = -18.0 // default setpoint
	failRampCPM  = 0.3   // °C per minute with a dead compressor
	doorRampCPM  = 0.1   // °C per minute with the door wedged open
	// Sensor bias ramp. Distinct from the healthy thermostat by construction:
	// a healthy cabinet oscillates around ~1.4 °C of band, while a bias moves
	// the whole distribution one way at a rate the thermostat cannot produce.
	// At 0.25 °C/min a 6-minute window shifts ~1.5 °C — comfortably outside
	// healthy's own spread, which is what makes the classes separable at the
	// window length the agent actually uses.
	driftCPM = -0.25 // sensor under-reporting ramp per minute

	// doorEventC is the per-tick warming while a staff door is open. Kept at
	// roughly one thermostat band over the event so the cabinet recovers
	// instead of running hot for ever.
	doorEventC = 0.05

	// ambientC bounds every warm ramp. A freezer cannot exceed room
	// temperature, and without a ceiling a long fault ran on to -2 °C, so a
	// 6-minute window could start anywhere on a 15 °C ramp and look like
	// almost anything. Tests were then measuring ramp phase, not fault type.
	ambientC = 5.0

	// Compressor control band. A real freezer thermostat has hysteresis: it
	// cuts in once the cabinet drifts above the band and pulls back to
	// setpoint. Without a band the simulator idled at ~3% duty — within noise
	// of a powered-off cabinet — so "healthy" and "power_outage" produced
	// physically identical telemetry and no classifier could tell them apart.
	// The band must be WIDER than both per-tick rates or the thermostat
	// chatters: with band < pull the controller unlatches inside a single
	// cooling step and switches on/off every reading, which looked like a
	// 4°C-wide sawtooth instead of a tight, slow cycle.
	coolOnAboveC = 0.15  // degC band, symmetric about setpoint
	coolPullK    = 0.006 // degC per tick while pulling down
	idleGainC    = 0.006 // degC per tick while idle (~50% duty)
)

// Reading is one sensor sample (JSON-encoded over MQTT).
type Reading struct {
	TS                time.Time `json:"ts"`
	DeviceID          string    `json:"deviceId"`
	TempC             float64   `json:"tempC"`
	PowerDraw         float64   `json:"powerDraw"`
	CompressorRunning bool      `json:"compressorRunning"`
	DoorOpen          bool      `json:"doorOpen"`
	Seq               int64     `json:"seq"`
}

// Ack is the device-side acknowledgement of a command.
type Ack struct {
	CommandID string    `json:"commandId"`
	DeviceID  string    `json:"deviceId"`
	Action    Action    `json:"action"`
	OK        bool      `json:"ok"`
	Detail    string    `json:"detail,omitempty"`
	At        time.Time `json:"at"`
}

// Device is a simulated freezer.
type Device struct {
	ID string

	mu                sync.Mutex
	faulted           string // current fault mode
	setpoint          float64
	defrostUntil      time.Time
	sensorOffsetC     float64 // reported = actual + offset (sensor_drift)
	tempC             float64 // actual cabinet temperature
	cooling           bool    // thermostat latch (hysteresis state)
	doorTicks         int     // remaining ticks of an open-door event
	compressorRunning bool    // controller is CALLING for the compressor
	motorRunning      bool    // motor is actually energized and doing work
	doorOpen          bool
	seq               int64
	rng               *rand.Rand
	log               *slog.Logger
}

// New starts a device at normal temperature with a time-based RNG seed.
func New(id string, log *slog.Logger) *Device {
	return newWithSeed(id, log, time.Now().UnixNano()+int64(len(id))*7919)
}

// NewSeeded starts a device with a fixed RNG seed. Tests use it for a
// reproducible noise sequence; production callers should use New.
func NewSeeded(id string, log *slog.Logger, seed int64) *Device {
	return newWithSeed(id, log, seed)
}

// newWithSeed is the single construction path: the RNG is fixed before the
// device is published, so no code can observe a device with the wrong seed.
func newWithSeed(id string, log *slog.Logger, seed int64) *Device {
	return &Device{
		ID:                id,
		setpoint:          normalTarget,
		tempC:             normalTarget,
		cooling:           true, // starts above band: pull down on first tick
		compressorRunning: true,
		motorRunning:      true,
		rng:               rand.New(rand.NewSource(seed)),
		log:               log,
	}
}

// Reset restores the pristine demo state: fault-free, door closed, sensor
// calibrated, cabinet at the default setpoint, compressor ready. Used by the
// operator's fleet reset (rehearsal cleanup), never by AI remediation: a real
// compressor_restart must let the cabinet pull down gradually so judges see
// the recovery arc instead of a teleport.
func (d *Device) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.faulted = FaultNone
	d.setpoint = normalTarget
	d.tempC = normalTarget
	d.sensorOffsetC = 0
	d.doorOpen = false
	d.doorTicks = 0
	d.defrostUntil = time.Time{}
	d.cooling = true
	d.compressorRunning = true
	d.motorRunning = true
}

// SetFaultMode enters/exits compressor_failure mode (compatibility wrapper).
func (d *Device) SetFaultMode(on bool) {
	if on {
		d.SetFault(FaultCompressorFailure)
	} else {
		d.SetFault(FaultNone)
	}
}

// SetFault switches the device into a fault mode ("" = healthy).
func (d *Device) SetFault(fault string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.faulted == fault {
		return
	}
	d.faulted = fault
	d.sensorOffsetC = 0
	d.motorRunning = true
	switch fault {
	case FaultCompressorFailure:
		d.compressorRunning = false
		d.log.Error("compressor_fail", "event", "compressor_fail", "tempC", round2(d.tempC))
	case FaultDoorLeftOpen:
		d.doorOpen = true
		d.compressorRunning = true
		d.log.Error("door_left_open", "event", "door_left_open", "tempC", round2(d.tempC))
	case FaultSensorDrift:
		// the sensor begins under-reporting — spoilage risk hides behind a
		// "healthy" reading; the AI must catch the drift from the pattern
		d.log.Error("sensor_drift", "event", "sensor_drift", "tempC", round2(d.tempC))
	case FaultPowerOutage:
		d.compressorRunning = false
		d.log.Error("power_outage", "event", "power_outage", "tempC", round2(d.tempC))
	default:
		d.compressorRunning = false
		d.doorOpen = false
		d.log.Info("fault_cleared", "event", "fault_cleared", "tempC", round2(d.tempC))
	}
}

// Faulted reports the current fault mode ("" = healthy).
func (d *Device) Faulted() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.faulted
}

// HandleCommand applies one control command and returns an ack payload.
func (d *Device) HandleCommand(commandID string, action Action, payload []byte) Ack {
	ack := Ack{CommandID: commandID, DeviceID: d.ID, Action: action, OK: true, At: time.Now().UTC()}
	switch action {
	case ActionCompressorRestart, ActionReboot:
		d.SetFault(FaultNone)
		ack.Detail = "fault cleared, device back online"
	case ActionSetFault:
		var p struct {
			Fault string `json:"fault"`
		}
		_ = json.Unmarshal(payload, &p)
		d.SetFault(p.Fault)
		ack.Detail = "fault mode set to " + p.Fault
	case ActionSetSetpoint:
		var p struct {
			Setpoint float64 `json:"setpoint"`
		}
		if err := json.Unmarshal(payload, &p); err != nil || p.Setpoint > 0 || p.Setpoint < -30 {
			ack.OK = false
			ack.Detail = "invalid setpoint (must be <=0, >=-30)"
			return ack
		}
		d.mu.Lock()
		d.setpoint = p.Setpoint
		d.mu.Unlock()
		ack.Detail = fmt.Sprintf("setpoint now %.1f°C", p.Setpoint)
	case ActionDefrost:
		d.mu.Lock()
		d.defrostUntil = time.Now().Add(2 * time.Minute)
		d.compressorRunning = false
		d.mu.Unlock()
		ack.Detail = "defrost cycle for 2m"
	case ActionEnterCompressorFailure:
		d.SetFault(FaultCompressorFailure)
		ack.Detail = "compressor failure injected"
	case ActionDemoReset:
		d.Reset()
		ack.Detail = "demo state reset"
	default:
		ack.OK = false
		ack.Detail = "unknown action"
	}
	return ack
}

// Tick advances the physics one interval step and returns the reading to
// publish.
func (d *Device) Tick() Reading {
	d.mu.Lock()
	defer d.mu.Unlock()

	inDefrost := time.Now().Before(d.defrostUntil)
	switch d.faulted {
	case FaultCompressorFailure:
		// Dead compressor: motor is not energized (motorRunning=false) but the
		// controller is still powered and calling for it, so the cabinet
		// drifts toward ambient at ~failRampCPM.
		d.tempC += failRampCPM / 12.0 // one 5s step
		if d.tempC > ambientC {
			d.tempC = ambientC
		}
		d.compressorRunning = false
		d.motorRunning = false
		if d.rng.Float64() < 0.05 {
			d.log.Warn("compressor_fail", "event", "compressor_fail_repeat",
				"tempC", round2(d.tempC), "msg", "compressor still unresponsive")
		}
	case FaultDoorLeftOpen:
		d.doorOpen = true
		d.tempC += doorRampCPM / 12.0
		if d.tempC > ambientC {
			d.tempC = ambientC
		}
		d.compressorRunning = true // compressor fighting a losing battle
		d.motorRunning = true
	case FaultSensorDrift:
		// reality: still cold (healthy physics); the sensor under-reports
		// more and more
		d.coolNormal(inDefrost)
		d.sensorOffsetC += driftCPM / 12.0
	case FaultPowerOutage:
		d.compressorRunning = false
		d.motorRunning = false
		d.tempC += (failRampCPM / 2) / 12.0 // slower ramp, no compressor at all
		if d.tempC > ambientC {
			d.tempC = ambientC
		}
	default:
		d.coolNormal(inDefrost)
		// Occasional door opening by staff. The warming must stay comparable to
		// the thermostat band: an earlier version added a flat +0.5 °C per tick
		// for 3 ticks (1.5 °C) against a 0.15 °C band, so a single door event
		// lifted the cabinet ten bands high and the compressor then ran forever
		// trying to recover — the fleet looked permanently warm.
		if d.doorOpen {
			d.doorTicks--
			d.tempC += doorEventC
			if d.doorTicks <= 0 {
				d.doorOpen = false
			}
		} else if d.rng.Float64() < 0.004 {
			d.doorOpen = true
			d.doorTicks = 3
			d.tempC += doorEventC
		}
	}

	// Sensor noise ±0.05 — small enough that the 5-min $derivative slope of
	// a healthy device stays clear of the +0.10°C/min AT_RISK trigger.
	reported := d.tempC + d.sensorOffsetC + d.rng.Float64()*0.1 - 0.05
	d.seq++

	// Power draw follows the MOTOR, not the controller's demand: a failed or
	// unpowered compressor is called but not energized, so it draws nothing.
	draw := 0.0
	if d.motorRunning && d.faulted != FaultPowerOutage {
		draw = 380 + d.rng.Float64()*40
	}
	return Reading{
		TS:                time.Now().UTC(),
		DeviceID:          d.ID,
		TempC:             round2(reported),
		PowerDraw:         round2(draw),
		CompressorRunning: d.compressorRunning,
		DoorOpen:          d.doorOpen,
		Seq:               d.seq,
	}
}

// coolNormal is the healthy cooling physics around the setpoint.
func (d *Device) coolNormal(inDefrost bool) {
	if inDefrost {
		// defrost: compressor off, slight warm drift
		d.compressorRunning = false
		d.tempC += 0.02
		return
	}
	// Thermostat with hysteresis, symmetric about the setpoint. The latch is
	// evaluated AFTER the temperature moves, so a step that crosses the
	// threshold is always seen — the earlier version clamped the coast to the
	// cut-in point and then never re-checked, so the compressor could stay off
	// (or on) indefinitely and the cabinet was not actually regulated.
	//
	// The band (lo..hi) is what makes the cabinet stationary: over a full
	// cycle the pull-down and the coast are symmetric, so the mean is the
	// setpoint and the trend over a multi-minute window averages out. That
	// matters beyond realism — an operator dashboard flags a warmed cabinet on
	// its slope, so a cycle that does not average to zero would show a healthy
	// fleet as permanently "at risk".
	lo := d.setpoint - coolOnAboveC/2
	hi := d.setpoint + coolOnAboveC/2
	if d.cooling {
		d.tempC -= coolPullK
		if d.tempC <= lo {
			d.tempC = lo
			d.cooling = false
		}
	} else {
		d.tempC += idleGainC
		if d.tempC >= hi {
			d.tempC = hi
			d.cooling = true
		}
	}
	d.compressorRunning = d.cooling
	d.motorRunning = d.cooling
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
