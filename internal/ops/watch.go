// Package ops — per-collection change-stream watcher (see hub.go).
package ops

import (
	"context"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// watchOne tails one collection's change stream until the context ends,
// resuming after resume tokens are lost (e.g. Mongo restarts).
func watchOne(ctx context.Context, log *slog.Logger, db *mongo.Database, h *hub, src streamSource) {
	coll := db.Collection(src.coll)
	for attempt := 0; ; attempt++ {
		cs, err := coll.Watch(ctx, mongo.Pipeline{},
			options.ChangeStream().
				SetFullDocument(options.UpdateLookup).
				SetMaxAwaitTime(5*time.Second))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("change stream open failed", "coll", src.coll, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff(attempt)):
			}
			continue
		}
		attempt = 0
		for cs.Next(ctx) {
			var ev struct {
				OperationType string   `bson:"operationType"`
				FullDocument  bson.Raw `bson:"fullDocument"`
			}
			if err := cs.Decode(&ev); err != nil {
				continue
			}
			switch ev.OperationType {
			case "insert", "update", "replace", "delete":
			default:
				continue // ignore heartbeats / internal ops
			}
			ce := ChangeEvent{
				Source: src.name,
				Op:     ev.OperationType,
				At:     time.Now().UTC(),
			}
			if ev.FullDocument != nil {
				ce.DeviceID = src.deviceID(ev.FullDocument)
				if doc, ok := src.doc(ev.FullDocument); ok {
					ce.Doc = doc
				}
			}
			h.publish(ce)
		}
		if ctx.Err() != nil {
			return
		}
		_ = cs.Close(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff(attempt)):
		}
	}
}

func backoff(attempt int) time.Duration {
	d := time.Duration(attempt+1) * time.Second
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	return d
}
