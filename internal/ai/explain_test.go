package ai

import (
	"testing"
)

// TopFactors must return at most k features ranked by |contribution| and mark
// direction consistently with the sign of the contribution.
func TestTopFactors(t *testing.T) {
	c := NewClassifier(testLogger())
	x := make([]float64, NumFeatures)
	for j := range x {
		x[j] = 0.5 * float64(j+1) // increasing z-scores
	}
	factors := c.TopFactors(x, "compressor_failure", 3)
	if len(factors) != 3 {
		t.Fatalf("want 3 factors, got %d", len(factors))
	}
	for i := 1; i < len(factors); i++ {
		a, b := abs(factors[i-1].Contribution), abs(factors[i].Contribution)
		if a < b {
			t.Errorf("factors not sorted by |contribution|: %v before %v", a, b)
		}
	}
	for _, f := range factors {
		if f.Direction != "raises" && f.Direction != "lowers" {
			t.Errorf("bad direction %q", f.Direction)
		}
		if (f.Contribution >= 0) != (f.Direction == "raises") {
			t.Errorf("direction %q inconsistent with contribution %v", f.Direction, f.Contribution)
		}
	}
	// unknown class => nil
	if got := c.TopFactors(x, "nope", 3); got != nil {
		t.Errorf("unknown class should return nil, got %v", got)
	}
}
