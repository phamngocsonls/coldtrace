package ai

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// IncidentMemory finds the k most similar RESOLVED incidents to a feature
// vector, excluding one device (the source of the incident itself).
//
// Ranking is cosine over the 8-dim feature vectors, computed in-process
// (ai.Similarity + ai.TopSimilar). It needs no search index, so it runs on the
// same stock MongoDB replica set as the rest of the demo.
type IncidentMemory struct {
	coll *mongo.Collection
}

// NewIncidentMemory wires the memory to the ai_incidents collection.
func NewIncidentMemory(coll *mongo.Collection) *IncidentMemory {
	return &IncidentMemory{coll: coll}
}

// TopSimilar returns up to k similar past incidents, most similar first.
func (m *IncidentMemory) TopSimilar(ctx context.Context, feats []float64, excludeDevice string, k int) []SimilarMatch {
	if m == nil || m.coll == nil || len(feats) == 0 {
		return nil
	}
	// Candidate set is small by construction (resolved incidents only), so a
	// bounded scan plus in-process ranking beats maintaining a separate index.
	cur, err := m.coll.Find(ctx, bson.M{"status": "RESOLVED"}, options.Find().SetLimit(200))
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)

	var past []struct {
		DeviceID   string    `bson:"deviceId"`
		FaultClass string    `bson:"faultClass"`
		Features   []float64 `bson:"features"`
		OpenedAt   time.Time `bson:"openedAt"`
	}
	if err := cur.All(ctx, &past); err != nil {
		return nil
	}

	cands := make([]SimilarMatch, 0, len(past))
	vecs := make([][]float64, 0, len(past))
	for _, p := range past {
		if p.DeviceID == excludeDevice {
			continue // don't match the device against itself
		}
		cands = append(cands, SimilarMatch{DeviceID: p.DeviceID, FaultClass: p.FaultClass, OpenedAt: p.OpenedAt})
		vecs = append(vecs, p.Features)
	}
	return TopSimilar(feats, cands, vecs, k)
}
