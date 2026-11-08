// Package ops — rules.go: operator-authored threshold alert rules.
// A rule watches one metric of the device twin (reported.* fields or
// lastSeen age) and fires when the comparison holds; matches are written to
// the events log with a severity so the console feed + SSE surface them.
//
// This makes "monitor anything" credible beyond the AI path: operators
// define plain threshold rules, the ops-api evaluates them every 30s.
package ops

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Rule is one operator-authored alert rule.
type Rule struct {
	ID        bson.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	RuleID    string        `bson:"ruleId" json:"ruleId"`
	DeviceID  string        `bson:"deviceId,omitempty" json:"deviceId,omitempty"` // "" = all devices
	Metric    string        `bson:"metric" json:"metric"`                         // reported.tempC | reported.powerDraw | reported.compressorRunning | lastSeenAgeSec
	Op        string        `bson:"op" json:"op"`                                 // gt | lt | eq
	Threshold float64       `bson:"threshold" json:"threshold"`
	Severity  string        `bson:"severity" json:"severity"` // info | warning | critical
	Enabled   bool          `bson:"enabled" json:"enabled"`
	CreatedBy string        `bson:"createdBy" json:"createdBy"`
	CreatedAt time.Time     `bson:"createdAt" json:"createdAt"`
}

// RuleMetric is the set of watchable metrics.
var RuleMetrics = []string{
	"reported.tempC", "reported.powerDraw",
	"reported.compressorRunning", "lastSeenAgeSec",
}

// RuleOps is the set of supported comparisons.
var RuleOps = []string{"gt", "lt", "eq"}

// ValidRule checks metric/op/severity against the allowlists.
func ValidRule(r *Rule) error {
	if !contains(RuleMetrics, r.Metric) {
		return fmt.Errorf("metric must be one of %s", strings.Join(RuleMetrics, ", "))
	}
	if !contains(RuleOps, r.Op) {
		return fmt.Errorf("op must be one of %s", strings.Join(RuleOps, ", "))
	}
	switch r.Severity {
	case "info", "warning", "critical":
	default:
		return fmt.Errorf("severity must be info|warning|critical")
	}
	return nil
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// CreateRule inserts a rule (unique ruleId).
func (s *Store) CreateRule(ctx context.Context, r *Rule) error {
	_, err := s.db.Collection("alert_rules").InsertOne(ctx, r)
	return err
}

// DeleteRule removes a rule by ruleId.
func (s *Store) DeleteRule(ctx context.Context, ruleID string) error {
	_, err := s.db.Collection("alert_rules").DeleteOne(ctx, bson.M{"ruleId": ruleID})
	return err
}

// ListRules returns all rules.
func (s *Store) ListRules(ctx context.Context) ([]Rule, error) {
	cur, err := s.db.Collection("alert_rules").Find(ctx, bson.M{},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}))
	if err != nil {
		return nil, err
	}
	var out []Rule
	return out, cur.All(ctx, &out)
}

// EvaluateRules runs every enabled rule against the current twin state and
// writes an alert event when a rule matches. Edge-triggered: the event fires
// only on the transition clear -> holding (firedWhile tracks the HOLDING
// state per rule+device); while the condition keeps holding nothing re-fires,
// and the next breach after a recovery notifies again.
func (s *Store) EvaluateRules(ctx context.Context, log *slog.Logger) {
	rules, err := s.ListRules(ctx)
	if err != nil {
		log.Warn("rules list failed", "err", err)
		return
	}
	now := time.Now().UTC()
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		q := bson.M{}
		if r.DeviceID != "" {
			q["deviceId"] = r.DeviceID
		}
		cur, err := s.Twin.Find(ctx, q)
		if err != nil {
			continue
		}
		var twins []struct {
			DeviceID string    `bson:"deviceId"`
			Reported bson.M    `bson:"reported"`
			LastSeen time.Time `bson:"lastSeen"`
		}
		if err := cur.All(ctx, &twins); err != nil {
			continue
		}
		for _, t := range twins {
			val, ok := metricValue(t.Reported, t.LastSeen, r.Metric, now)
			key := r.RuleID + "|" + t.DeviceID
			holding := ok && matches(val, r.Op, r.Threshold)
			// dedupe map is touched here and by DeleteRule — hold the lock
			s.rulesMu.Lock()
			wasHolding := s.firedWhile[key]
			if !holding {
				delete(s.firedWhile, key) // recovered: arm the next breach
				s.rulesMu.Unlock()
				continue
			}
			if wasHolding {
				s.rulesMu.Unlock()
				continue // already alerted for this ongoing breach
			}
			s.firedWhile[key] = true
			s.rulesMu.Unlock()
			s.AddEvent(ctx, Event{
				Type:     "alert_rule",
				DeviceID: t.DeviceID,
				Message: fmt.Sprintf("rule %s: %s %s %.2f (value %.2f)",
					r.RuleID, r.Metric, r.Op, r.Threshold, val),
				Actor:    "rule:" + r.RuleID,
				Severity: r.Severity,
				At:       now,
			})
			log.Info("alert_rule_fired", "ruleId", r.RuleID, "deviceId", t.DeviceID,
				"metric", r.Metric, "value", val)
		}
	}
}

// metricValue extracts the watched metric from a twin doc.
func metricValue(reported bson.M, lastSeen time.Time, metric string, now time.Time) (float64, bool) {
	switch metric {
	case "lastSeenAgeSec":
		if lastSeen.IsZero() {
			return 0, false
		}
		return now.Sub(lastSeen).Seconds(), true
	default:
		if reported == nil {
			return 0, false
		}
		parts := strings.SplitN(metric, ".", 2)
		if len(parts) != 2 || parts[0] != "reported" {
			return 0, false
		}
		v, ok := reported[parts[1]]
		if !ok || v == nil {
			return 0, false
		}
		switch n := v.(type) {
		case float64:
			return n, true
		case bool:
			if n {
				return 1, true
			}
			return 0, true
		case int32:
			return float64(n), true
		case int64:
			return float64(n), true
		}
		return math.NaN(), false
	}
}

// matches applies the comparison.
func matches(val float64, op string, threshold float64) bool {
	switch op {
	case "gt":
		return val > threshold
	case "lt":
		return val < threshold
	case "eq":
		return val == threshold
	}
	return false
}
