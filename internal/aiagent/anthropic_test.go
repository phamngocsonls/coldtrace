package aiagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An Anthropic key must select the engine and route RCA through
// /v1/messages with the Messages API headers (not the OpenAI shape).
func TestRootCauseAnthropicEngine(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": `{"summary":"compressor tripped","rootCause":"duty cycle low",` +
					`"evidence":["power dropped"],"actions":["compressor_restart"]}`,
			}},
			"usage": map[string]int{"input_tokens": 5, "output_tokens": 6},
		})
	}))
	defer srv.Close()

	c := New(Config{AnthropicAPIKey: "sk-ant-test", AnthropicBaseURL: srv.URL, AnthropicModel: "claude-test"})
	if got := c.Engine(); got != "anthropic" {
		t.Fatalf("engine = %q, want anthropic", got)
	}
	if got := c.modelName(); got != "claude-test" {
		t.Errorf("modelName = %q", got)
	}

	// store=nil: facts stay empty, but the call still reaches the provider.
	rca, err := c.RootCause(context.Background(), nil, nil, [12]byte{})
	if err != nil {
		t.Fatalf("RootCause: %v", err)
	}
	if rca.Fallback {
		t.Fatalf("fell back to template: %s", rca.Error)
	}
	if rca.Summary != "compressor tripped" || rca.RootCause != "duty cycle low" {
		t.Errorf("rca = %+v", rca)
	}
	if rca.Model != "claude-test" {
		t.Errorf("audit model = %q", rca.Model)
	}
	if gotPath != "/v1/messages" || gotKey != "sk-ant-test" || gotVersion == "" {
		t.Errorf("path=%q key=%q version=%q", gotPath, gotKey, gotVersion)
	}
	if body["system"] == nil {
		t.Error("system prompt must be hoisted to the top-level field")
	}
	if _, ok := body["max_tokens"]; !ok {
		t.Error("max_tokens is required by the Messages API")
	}
}

// Streaming chat (copilot) has no Anthropic adapter yet; it must fail with a
// clear message instead of POSTing OpenAI JSON to /v1/messages.
func TestStreamingRefusesAnthropic(t *testing.T) {
	c := New(Config{AnthropicAPIKey: "k"})
	err := c.ChatStream(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "anthropic") {
		t.Fatalf("want explicit anthropic refusal, got %v", err)
	}
}

// Cursor still wins over Anthropic when both keys are set.
func TestEnginePrecedence(t *testing.T) {
	c := New(Config{CursorAPIKey: "cur", AnthropicAPIKey: "ant", APIKey: "oai"})
	if got := c.Engine(); got != "cursor" {
		t.Fatalf("engine = %q, want cursor", got)
	}
	c = New(Config{AnthropicAPIKey: "ant", APIKey: "oai"})
	if got := c.Engine(); got != "anthropic" {
		t.Fatalf("engine = %q, want anthropic over openai", got)
	}
	c = New(Config{APIKey: "oai"})
	if got := c.Engine(); got != "openai" {
		t.Fatalf("engine = %q, want openai", got)
	}
	if got := New(Config{}).Engine(); got != "none" {
		t.Fatalf("engine = %q, want none", got)
	}
}
