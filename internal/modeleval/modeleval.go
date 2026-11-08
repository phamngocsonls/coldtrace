// Package modeleval — offline accuracy evaluation for the ColdTrace
// classifier, shared by tools/evalmodel (CLI) and the ops-api /api/model/eval
// endpoint. Generates labeled windows per fault class from the same
// synthetic physics used for warm-start, classifies each, and reports
// overall accuracy plus per-class precision/recall/F1 and a confusion matrix.
package modeleval

import (
	"github.com/onemount/coldtrace/internal/ai"
)

// ClassMetrics is one row of per-class performance.
type ClassMetrics struct {
	Class     string  `json:"class"`
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
	Support   int     `json:"support"`
}

// Report is the full evaluation result.
type Report struct {
	Windows  int            `json:"windows"`
	Accuracy float64        `json:"accuracy"`
	Classes  []string       `json:"classes"`
	Matrix   [][]int        `json:"matrix"` // rows=true, cols=pred
	PerClass []ClassMetrics `json:"perClass"`
}

// Evaluate scores the warm-started classifier on windows from the LIVE
// SIMULATOR (internal/sim), not on the synthetic generators it was trained on.
//
// An earlier version drew a "held-out" set from the same generators with a
// different RNG seed and reported ~100% — which is circular: it measures
// whether the model can recognise its own training distribution, not whether
// it works. Scoring against real simulator physics is the number that matters,
// and it is the only one worth showing to an operator.
func Evaluate(nPerClass int) Report {
	classes := ai.Classes
	xs, ys := ai.BuildSimulatorEvalSet(nPerClass)

	cl := ai.NewClassifier(nil)
	confusion := make([][]int, len(classes))
	for k := range confusion {
		confusion[k] = make([]int, len(classes))
	}
	correct := 0
	for i := range xs {
		pred, _ := cl.Predict(xs[i])
		pi := indexOf(classes, pred)
		confusion[ys[i]][pi]++
		if pred == classes[ys[i]] {
			correct++
		}
	}

	perClass := make([]ClassMetrics, len(classes))
	for y := range classes {
		tp := confusion[y][y]
		fp, fn := 0, 0
		for k := range classes {
			if k != y {
				fp += confusion[k][y]
				fn += confusion[y][k]
			}
		}
		prec := ratio(tp, tp+fp)
		rec := ratio(tp, tp+fn)
		perClass[y] = ClassMetrics{
			Class: classes[y], Precision: prec, Recall: rec,
			F1: fdiv(2*prec*rec, prec+rec), Support: tp + fn,
		}
	}
	return Report{
		Windows: len(xs), Accuracy: ratio(correct, len(xs)),
		Classes: classes, Matrix: confusion, PerClass: perClass,
	}
}

func indexOf(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return -1
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func fdiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}
