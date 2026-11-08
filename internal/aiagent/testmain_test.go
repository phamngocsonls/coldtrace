// Shared test helpers: a lazily-connected mongo.Database pointing at a dead
// host — tool calls fail fast inside the 5s tool budget and the error rides
// back as a tool result, which is exactly the fallback path we want to
// exercise without standing up a live mongo in unit tests.
package aiagent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var (
	testDB  *mongo.Database
	testCtx = context.Background()
)

func fakeDB(t *testing.T) *mongo.Database {
	t.Helper()
	if testDB != nil {
		return testDB
	}
	client, err := mongo.Connect(options.Client().
		ApplyURI("mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=200&connectTimeoutMS=200"))
	if err != nil {
		t.Fatalf("mongo.Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	testDB = client.Database("coldtrace-test")
	return testDB
}

func unjson(raw string, v any) error { return json.Unmarshal([]byte(raw), v) }

var _ = time.Second // kept for clarity in future fixtures
