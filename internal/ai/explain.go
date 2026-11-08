package ai

import (
	"sort"
)

// TopFactor is one feature's directional contribution to the predicted class.
type TopFactor struct {
	Feature      string  `json:"feature"`
	Z            float64 `json:"z"`            // normalized feature value
	Weight       float64 `json:"weight"`       // class weight for this feature
	Contribution float64 `json:"contribution"` // weight * z (signed)
	Direction    string  `json:"direction"`    // "raises" or "lowers" the class score
}

// TopFactors returns the k most influential features behind a prediction of
// `class` for the normalized vector x, ranked by |contribution| descending.
// Contribution = w[class][j] * z[j] (the winning logit's linear terms).
func (c *Classifier) TopFactors(x []float64, class string, k int) []TopFactor {
	c.mu.Lock()
	defer c.mu.Unlock()
	classIdx := -1
	for i, cl := range Classes {
		if cl == class {
			classIdx = i
			break
		}
	}
	if classIdx < 0 {
		return nil
	}
	out := make([]TopFactor, 0, NumFeatures)
	for j := 0; j < NumFeatures && j < len(x); j++ {
		contrib := c.w[classIdx][j] * x[j]
		out = append(out, TopFactor{
			Feature:      FeatureNames[j],
			Z:            round4(x[j]),
			Weight:       round4(c.w[classIdx][j]),
			Contribution: round4(contrib),
			Direction:    map[bool]string{true: "raises", false: "lowers"}[contrib >= 0],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := abs(out[i].Contribution), abs(out[j].Contribution)
		if a == b {
			return out[i].Feature < out[j].Feature
		}
		return a > b
	})
	if k > 0 && k < len(out) {
		out = out[:k]
	}
	return out
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func round4(v float64) float64 {
	if v < 0 {
		return -float64(int(-v*10000+0.5)) / 10000
	}
	return float64(int(v*10000+0.5)) / 10000
}
