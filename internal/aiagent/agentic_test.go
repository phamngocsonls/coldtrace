// GroundedChat against a FAKE OpenAI-compatible gateway (httptest): asserts
// the wire sequence (assistant tool_calls before tool messages), the round
// cap, and that narration returned alongside tool calls reaches onDelta.
package aiagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeGateway asserts request shapes and returns scripted responses.
type fakeGateway struct {
	srv      *httptest.Server
	requests []chatRequestFull
	script   []func(w http.ResponseWriter, r *http.Request)
}

func newFakeGateway(t *testing.T, script []func(w http.ResponseWriter, r *http.Request)) *fakeGateway {
	fg := &fakeGateway{script: script}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req chatRequestFull
		_ = json.NewDecoder(r.Body).Decode(&req)
		fg.requests = append(fg.requests, req)
		step := fg.script[len(fg.requests)-1]
		step(w, r)
	})
	fg.srv = httptest.NewServer(mux)
	t.Cleanup(fg.srv.Close)
	return fg
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestGroundedChatRunsToolsAndAnswers(t *testing.T) {
	fg := newFakeGateway(t, []func(w http.ResponseWriter, r *http.Request){
		// turn 1: model asks for the fleet (with narration alongside)
		func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"content": "Checking the fleet.",
						"tool_calls": []map[string]any{{
							"id": "call_1", "type": "function",
							"function": map[string]any{"name": "get_fleet", "arguments": "{}"},
						}},
					},
				}},
			})
		},
		// turn 2: final answer
		func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": "All 12 devices are OK."}}},
			})
		},
	})
	c := New(Config{BaseURL: fg.srv.URL, APIKey: "k", Model: "m", Timeout: 5 * time.Second})
	db := fakeDB(t)
	var got []string
	err := c.GroundedChat(context.Background(), db, []ChatMessage{{Role: "user", Content: "status?"}}, func(d string) { got = append(got, d) })
	if err != nil {
		t.Fatalf("GroundedChat: %v", err)
	}
	if len(fg.requests) != 2 {
		t.Fatalf("turns = %d, want 2", len(fg.requests))
	}
	// narration streamed even though the turn also had tool calls
	if len(got) == 0 || got[0] != "Checking the fleet." {
		t.Fatalf("narration dropped, got %v", got)
	}
	if got[len(got)-1] != "All 12 devices are OK." {
		t.Fatalf("final answer missing, got %v", got)
	}
	// wire order: assistant tool_calls message precedes its tool result
	hist := fg.requests[1].Messages
	last := hist[len(hist)-1]
	if last.Role != "tool" || last.ToolCallID != "call_1" {
		t.Fatalf("last history message = %+v, want tool result for call_1", last)
	}
	if hist[len(hist)-2].Role != "assistant" || len(hist[len(hist)-2].ToolCalls) == 0 {
		t.Fatalf("assistant tool_calls message missing before tool result: %+v", hist[len(hist)-2])
	}
	// tool result content present
	if last.Content == "" {
		t.Fatalf("tool result content empty")
	}
}

func TestGroundedChatRoundCap(t *testing.T) {
	// the model keeps calling tools — the loop must stop at maxToolRounds
	script := []func(w http.ResponseWriter, r *http.Request){}
	for i := 0; i <= maxToolRounds+2; i++ {
		script = append(script, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{"tool_calls": []map[string]any{{
						"id": "c", "type": "function",
						"function": map[string]any{"name": "get_fleet", "arguments": "{}"},
					}}},
				}},
			})
		})
	}
	fg := newFakeGateway(t, script)
	c := New(Config{BaseURL: fg.srv.URL, APIKey: "k", Model: "m", Timeout: 5 * time.Second})
	err := c.GroundedChat(context.Background(), fakeDB(t), []ChatMessage{{Role: "user", Content: "?"}}, func(string) {})
	if err == nil {
		t.Fatalf("expected the round-cap error")
	}
	if len(fg.requests) != maxToolRounds {
		t.Fatalf("turns = %d, want exactly maxToolRounds (%d)", len(fg.requests), maxToolRounds)
	}
}

func TestChatStreamSurfacesDecodeErrors(t *testing.T) {
	// a truncated stream must surface an error, not fake success
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"hel`)) // cut mid-JSON
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: 5 * time.Second})
	err := c.ChatStream(context.Background(), []ChatMessage{{Role: "user", Content: "?"}}, func(string) {})
	if err == nil {
		t.Fatalf("truncated stream must return an error")
	}
}
