package ops

import (
	"context"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// runExpr evaluates an aggregation pipeline against a single synthetic
// document and returns the resulting `status` field.
//
// It needs a live MongoDB because the point is to exercise the server's own
// `$switch`/`$ifNull`/missing-field semantics. Those are exactly where the
// fleet false positive came from — a Go reimplementation would have happily
// agreed with the buggy pipeline. The test skips (rather than fails) when no
// database is reachable, so `go test ./...` still works without a stack.
func runExpr(t *testing.T, pipeline bson.A, doc bson.M) string {
	t.Helper()

	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		uri = "mongodb://localhost:27017"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetServerSelectionTimeout(2 * time.Second))
	if err != nil {
		t.Skipf("no MongoDB at %s: %v", uri, err)
	}
	defer client.Disconnect(context.Background())
	if err := client.Ping(ctx, nil); err != nil {
		t.Skipf("no MongoDB at %s: %v", uri, err)
	}

	coll := client.Database("coldtrace_test").Collection("status_expr")
	if err := coll.Drop(ctx); err != nil {
		t.Fatalf("drop: %v", err)
	}
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_ = coll.Drop(c)
	})

	full := append(bson.A{bson.D{{Key: "$documents", Value: bson.A{doc}}}}, pipeline...)
	// $documents is only valid at database/cluster level, so aggregate the
	// database with a dummy collection argument rather than the collection.
	cur, err := client.Database("coldtrace_test").Aggregate(ctx, full)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var out []struct {
		Status string `bson:"status"`
	}
	if err := cur.All(ctx, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 result, got %d", len(out))
	}
	return out[0].Status
}
