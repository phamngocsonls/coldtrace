// Package ai — hybrid autonomy policy: proposes on unknown classes, learns
// approvals, then auto-remediates within strict guardrails.
package ai

import (
	"time"
)

// Decision is the policy outcome for one detected incident.
type Decision string

const (
	// DecisionPropose — first time seeing this class: AI proposes, human approves.
	DecisionPropose Decision = "PROPOSED"
	// DecisionAuto — approved class + guardrails passed: agent remediates itself.
	DecisionAuto Decision = "AUTO"
	// DecisionHold — approved class but a guardrail failed: propose with reason.
	DecisionHold Decision = "HOLD"
)

// PolicyInput is what the evaluator needs to decide.
type PolicyInput struct {
	DeviceID    string
	FaultClass  string
	Confidence  float64
	EtaAlarmMin float64              // NaN when not on a rising path
	AlarmingNow int                  // devices currently in ALARM
	LastAutoFor map[string]time.Time // deviceID -> last auto-remediation time
	Now         time.Time
}

// PolicyConfig tunes the guardrails.
type PolicyConfig struct {
	MinConfidence       float64       // 0.85
	MaxEtaAlarmMinutes  float64       // 45
	AutoCooldown        time.Duration // 30m per device
	FleetCircuitBreaker int           // suspend auto if >= N devices ALARM
}

// DefaultPolicy is the hackathon demo configuration.
//
// The ETA horizon is 45 minutes, not 10: a compressor failure at the -18 °C
// setpoint warms at ~0.3 °C/min, so time-to-alarm is ~30-36 minutes at the
// moment the classifier is confident. A 10-minute horizon would HOLD every
// detection until the cabinet was already near excursion — the opposite of
// what the guardrail is for. 45 minutes still rejects a slow drift that has
// no chance of breaching during the shift.
func DefaultPolicy() PolicyConfig {
	return PolicyConfig{
		MinConfidence:       0.85,
		MaxEtaAlarmMinutes:  45,
		AutoCooldown:        30 * time.Minute,
		FleetCircuitBreaker: 2,
	}
}

// PolicyResult is the decision plus human-readable reasoning (for logs and
// the incident record).
type PolicyResult struct {
	Decision Decision
	Reason   string
	ConfGate bool
	EtaGate  bool
	Cooldown bool
	Circuit  bool
}

// Evaluate applies the hybrid autonomy policy.
func Evaluate(in PolicyInput, cfg PolicyConfig, approvedClasses map[string]bool) PolicyResult {
	r := PolicyResult{}
	if !approvedClasses[in.FaultClass] {
		r.Decision = DecisionPropose
		r.Reason = "first incident of class " + in.FaultClass + " — awaiting human approval to enable auto-remediation"
		return r
	}
	r.ConfGate = in.Confidence >= cfg.MinConfidence
	if in.EtaAlarmMin == in.EtaAlarmMin { // not NaN
		r.EtaGate = in.EtaAlarmMin <= cfg.MaxEtaAlarmMinutes
	}
	// fleet circuit breaker
	r.Circuit = in.AlarmingNow >= cfg.FleetCircuitBreaker
	// cooldown
	if lt, ok := in.LastAutoFor[deviceKey(in)]; ok && in.Now.Sub(lt) < cfg.AutoCooldown {
		r.Cooldown = true
	}

	switch {
	case r.Circuit:
		r.Decision = DecisionHold
		r.Reason = "fleet circuit breaker: too many devices in ALARM — auto-actions suspended, human review required"
	case r.Cooldown:
		r.Decision = DecisionHold
		r.Reason = "auto-remediation cooldown active for this device"
	case !r.ConfGate:
		r.Decision = DecisionHold
		r.Reason = "confidence below gate"
	case !r.EtaGate:
		r.Decision = DecisionHold
		r.Reason = "time-to-alarm beyond horizon"
	default:
		r.Decision = DecisionAuto
		r.Reason = "approved class, confidence and ETA gates passed within guardrails"
	}
	return r
}

func deviceKey(in PolicyInput) string { return in.DeviceID }
