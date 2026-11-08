// Package ai — incident memory: cosine ranking over past feature vectors (see memory.go).
package ai

import "time"

// SimilarMatch is one similar past incident (agent-side struct; used by both
// incident memory in memory.go).
type SimilarMatch struct {
	DeviceID   string    `json:"deviceId"`
	FaultClass string    `json:"faultClass"`
	Score      float64   `json:"score"`
	OpenedAt   time.Time `json:"openedAt"`
}

// TopSimilar ranks candidates by cosine similarity to v.
// Insertion sort keeps ties stable; candidate lists are small (<200).
func TopSimilar(v []float64, cands []SimilarMatch, vecs [][]float64, k int) []SimilarMatch {
	for i := range cands {
		cands[i].Score = Similarity(v, vecs[i])
	}
	for i := 1; i < len(cands); i++ {
		for j := i; j > 0 && cands[j].Score > cands[j-1].Score; j-- {
			cands[j], cands[j-1] = cands[j-1], cands[j]
		}
	}
	if k > 0 && k < len(cands) {
		cands = cands[:k]
	}
	return cands
}
