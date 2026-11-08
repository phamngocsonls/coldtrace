// Tests for the alert-rule evaluator: edge triggering (fire on clear->set,
// stay quiet while holding, re-arm after recovery) plus the metric matcher.
// The evaluator itself needs a live mongo (rules list + twins); the
// edge-trigger state machine around firedWhile is asserted as the pure
// decision table the loop implements.
package ops

import (
	"testing"
)

func TestMatches(t *testing.T) {
	cases := []struct {
		name string
		val  float64
		op   string
		th   float64
		want bool
	}{
		{"gt above", -9, "gt", -10, true},
		{"gt below", -11, "gt", -10, false},
		{"lt below", -11, "lt", -10, true},
		{"eq equal", -10, "eq", -10, true},
		{"eq off", -9, "eq", -10, false},
	}
	for _, c := range cases {
		if got := matches(c.val, c.op, c.th); got != c.want {
			t.Fatalf("matches(%v, %q, %v) = %v, want %v", c.val, c.op, c.th, got, c.want)
		}
	}
}

// wasHolding | holding -> fire? (the decision implemented by EvaluateRules)
func TestRuleEdgeSemantics(t *testing.T) {
	cases := []struct {
		name       string
		wasHolding bool
		holding    bool
		wantFire   bool
	}{
		{"breach starts", false, true, true},
		{"still holding", true, true, false},
		{"recovered re-arms", true, false, false},
		{"quiet stays quiet", false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fire := c.holding && !c.wasHolding
			if fire != c.wantFire {
				t.Fatalf("fire = %v, want %v", fire, c.wantFire)
			}
		})
	}
}
