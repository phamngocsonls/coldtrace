package ai

import (
	"context"
	"testing"
)

// Incident memory ranks in-process by cosine over the 8-dim feature vectors.
// No search index and no Atlas: it must order by similarity, honour k, and
// skip the source device.
func TestIncidentMemoryRanksByCosine(t *testing.T) {
	cands := []SimilarMatch{
		{DeviceID: "FZ-001", FaultClass: "compressor_failure"},
		{DeviceID: "FZ-002", FaultClass: "compressor_failure"},
		{DeviceID: "FZ-003", FaultClass: "door_open"},
	}
	vecs := [][]float64{
		{1, 0, 0},
		{0.9, 0.1, 0},
		{0, 1, 0},
	}
	got := TopSimilar([]float64{1, 0, 0}, cands, vecs, 2)
	if len(got) != 2 {
		t.Fatalf("want 2 results, got %d", len(got))
	}
	if got[0].DeviceID != "FZ-001" || got[1].DeviceID != "FZ-002" {
		t.Fatalf("wrong order: %s, %s", got[0].DeviceID, got[1].DeviceID)
	}
	if got[0].Score < got[1].Score {
		t.Fatal("scores not descending")
	}
}

// k larger than the candidate set returns everything; k=0 keeps all.
func TestTopSimilarBoundsK(t *testing.T) {
	cands := []SimilarMatch{{DeviceID: "A"}, {DeviceID: "B"}}
	vecs := [][]float64{{1, 0}, {0, 1}}
	if got := TopSimilar([]float64{1, 0}, cands, vecs, 0); len(got) != 2 {
		t.Fatalf("k=0 should keep all, got %d", len(got))
	}
	if got := TopSimilar([]float64{1, 0}, cands, vecs, 10); len(got) != 2 {
		t.Fatalf("k>len should keep all, got %d", len(got))
	}
}

// Empty input and an unconfigured memory must be safe no-ops, not panics.
func TestIncidentMemoryEmptyInputs(t *testing.T) {
	var nilMem *IncidentMemory
	if got := nilMem.TopSimilar(context.Background(), []float64{1, 0}, "", 3); got != nil {
		t.Fatalf("nil memory should return nil, got %v", got)
	}
	m := NewIncidentMemory(nil)
	if got := m.TopSimilar(context.Background(), []float64{1, 0}, "", 3); got != nil {
		t.Fatalf("nil collection should return nil, got %v", got)
	}
	if got := m.TopSimilar(context.Background(), nil, "", 3); got != nil {
		t.Fatalf("empty features should return nil, got %v", got)
	}
}
