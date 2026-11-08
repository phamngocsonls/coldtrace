package ai

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
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

// The incident document is read by the console through ops-api, which does
// not decode into this struct. Stored similar matches must therefore use the
// same camelCase keys the console reads; a struct with only json tags used to
// persist deviceid/faultclass/openedat and the panel rendered blank.
func TestSimilarMatchBSONKeys(t *testing.T) {
	b, err := bson.Marshal(SimilarMatch{
		DeviceID: "FZ-004", FaultClass: "door_left_open", Score: 0.91,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got bson.M
	if err := bson.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"deviceId", "faultClass", "score", "openedAt"} {
		if _, ok := got[k]; !ok {
			t.Errorf("BSON key %q missing (got %v)", k, got)
		}
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
