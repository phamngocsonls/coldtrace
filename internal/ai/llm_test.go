package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The RCA client must speak the OpenAI chat-completions wire format against a
// configurable base URL (api.openai.com default, OneNexus catalog override)
// and carry the bearer key on every call.
func TestChatComplete(t *testing.T) {
	var gotPath, gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			Model    string        `json:"model"`
			Messages []ChatMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		if len(req.Messages) != 2 {
			t.Errorf("want 2 messages, got %d", len(req.Messages))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "ok analysis"}}},
			"usage":   map[string]int{"prompt_tokens": 11, "completion_tokens": 7},
		})
	}))
	defer srv.Close()

	cl := NewLLM(LLMConfig{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"})
	out, usage, err := cl.ChatComplete(context.Background(), []ChatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "why did FZ-003 fail?"},
	})
	if err != nil {
		t.Fatalf("ChatComplete: %v", err)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotModel != "test-model" {
		t.Errorf("model = %q", gotModel)
	}
	if out != "ok analysis" {
		t.Errorf("content = %q", out)
	}
	if usage.PromptTokens != 11 || usage.CompletionTokens != 7 {
		t.Errorf("usage = %d/%d", usage.PromptTokens, usage.CompletionTokens)
	}
}

// Empty content must surface as an error, not a silent empty RCA.
func TestChatCompleteEmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{}})
	}))
	defer srv.Close()

	cl := NewLLM(LLMConfig{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	if _, _, err := cl.ChatComplete(context.Background(), []ChatMessage{{Role: "user", Content: "x"}}); err == nil {
		t.Errorf("want error on empty choices")
	}
}

// An Anthropic key switches the RCA client to /v1/messages with the Messages
// API headers, and reports the provider for startup logs / audit fields.
func TestChatCompleteAnthropic(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": `{"summary":"compressor tripped"}`}},
			"usage":   map[string]int{"input_tokens": 21, "output_tokens": 9},
		})
	}))
	defer srv.Close()

	cl := NewLLM(LLMConfig{
		// An OpenAI key is also present: the Anthropic key must win.
		BaseURL: "http://should-not-be-used.invalid", APIKey: "sk-openai", Model: "m",
		AnthropicBaseURL: srv.URL, AnthropicAPIKey: "sk-ant", AnthropicModel: "claude-test",
	})
	if cl.Provider() != "anthropic" {
		t.Fatalf("provider = %q", cl.Provider())
	}
	if cl.Model() != "claude-test" {
		t.Errorf("model = %q", cl.Model())
	}
	out, usage, err := cl.ChatComplete(context.Background(), []ChatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "explain"},
	})
	if err != nil {
		t.Fatalf("ChatComplete: %v", err)
	}
	if out != `{"summary":"compressor tripped"}` {
		t.Errorf("content = %q", out)
	}
	if usage.PromptTokens != 21 || usage.CompletionTokens != 9 {
		t.Errorf("usage = %d/%d", usage.PromptTokens, usage.CompletionTokens)
	}
	if gotPath != "/v1/messages" || gotKey != "sk-ant" || gotVersion == "" {
		t.Errorf("path=%q key=%q version=%q", gotPath, gotKey, gotVersion)
	}
}
