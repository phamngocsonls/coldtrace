// Package ops — SSE hub: fans MongoDB change streams out to all connected
// Ops Consoles. One goroutine per collection stream; every client gets a
// filtered view (device query param supported for the device page).
package ops

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// rawDocJSON converts a full raw BSON document to JSON for SSE payloads.
func rawDocJSON(d bson.Raw) (json.RawMessage, bool) {
	if len(d) == 0 {
		return nil, false
	}
	var m bson.M
	if err := bson.Unmarshal(d, &m); err != nil {
		return nil, false
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return out, true
}

// ChangeEvent is the SSE payload shape (a curated subset of the MongoDB
// change event + the full document on inserts).
type ChangeEvent struct {
	Source   string          `json:"source"` // device_state | ai_incidents | events
	Op       string          `json:"op"`     // insert | update | replace | delete
	DeviceID string          `json:"deviceId,omitempty"`
	Doc      json.RawMessage `json:"doc,omitempty"`
	At       time.Time       `json:"at"`
}

// hub fans change events out to SSE subscribers.
type hub struct {
	log  *slog.Logger
	mu   sync.RWMutex
	subs map[chan ChangeEvent]struct{}
}

// Hub is the public alias used by cmd/ops-api.
type Hub = hub

// NewHub builds the SSE fan-out hub.
func NewHub(log *slog.Logger) *hub {
	return &hub{log: log, subs: map[chan ChangeEvent]struct{}{}}
}

func (h *hub) subscribe() chan ChangeEvent {
	ch := make(chan ChangeEvent, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan ChangeEvent) {
	h.mu.Lock()
	delete(h.subs, ch)
	close(ch)
	h.mu.Unlock()
}

func (h *hub) publish(ev ChangeEvent) {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default: // slow client — drop rather than block the stream
		}
	}
}

// Publish exposes event fan-out to other packages (public wrapper).
func (h *hub) Publish(ev ChangeEvent) { h.publish(ev) }

// sseHeaders writes the SSE response head.
func (h *hub) SSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

func (h *hub) Flusher(w http.ResponseWriter) http.Flusher {
	f, _ := w.(http.Flusher)
	return f
}

// serveHTTP is the GET /api/stream SSE endpoint.
func (h *hub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	h.SSEHeaders(w)
	flusher := h.Flusher(w)
	if flusher == nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := h.subscribe()
	defer h.unsubscribe(ch)

	// first comment so the browser EventSource fires open immediately
	if _, err := w.Write([]byte(": connected\n\n")); err != nil {
		return
	}
	flusher.Flush()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case ev := <-ch:
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := w.Write(append([]byte("data: "), b...)); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// ServeHTTP exposes the SSE stream (public wrapper).
func (h *hub) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.serveHTTP(w, r) }

// streamSource is one watched collection; see WatchChangeStreams.
type streamSource struct {
	name     string
	coll     string
	deviceID func(doc bson.Raw) string
	doc      func(doc bson.Raw) (json.RawMessage, bool)
}

// WatchChangeStreams tails the operational collections and publishes to the
// hub (public entry point; see watch.go for the per-collection watcher).
// device_state is a plain collection so its stream carries the full doc;
// events/ai_incidents likewise. This is the demo's "MongoDB as message bus"
// moment: no polling anywhere in the UI.
func WatchChangeStreams(ctx context.Context, log *slog.Logger, db *mongo.Database, h *hub) {
	lookupString := func(d bson.Raw, field string) string {
		// SafeString lookup: StringValue() panics if the field is missing or
		// not a string (e.g. delete events, FLEET pseudo-device docs).
		if v := d.Lookup(field); v.Type == bson.TypeString {
			return v.StringValue()
		}
		return ""
	}
	sources := []streamSource{
		{
			name: "device_state", coll: "device_state",
			deviceID: func(d bson.Raw) string { return lookupString(d, "deviceId") },
			doc: func(d bson.Raw) (json.RawMessage, bool) {
				// payload: the reported subdoc as JSON
				var doc struct {
					Reported bson.M `bson:"reported"`
				}
				if err := bson.Unmarshal(d, &doc); err != nil || doc.Reported == nil {
					return nil, false
				}
				out, err := json.Marshal(doc.Reported)
				if err != nil {
					return nil, false
				}
				return out, true
			},
		},
		{
			name: "ai_incidents", coll: "ai_incidents",
			deviceID: func(d bson.Raw) string { return lookupString(d, "deviceId") },
			doc:      rawDocJSON,
		},
		{
			name: "events", coll: "events",
			deviceID: func(d bson.Raw) string { return lookupString(d, "deviceId") },
			doc:      rawDocJSON,
		},
	}
	for _, src := range sources {
		go watchOne(ctx, log, db, h, src)
	}
}
