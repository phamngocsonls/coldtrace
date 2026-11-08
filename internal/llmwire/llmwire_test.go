package llmwire

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, b)
	}
	return m
}

// The Anthropic engine must speak the Messages API: /v1/messages, x-api-key,
// anthropic-version, system hoisted out of messages, reply read from content[].
func TestAnthropicWire(t *testing.T) {
	var gotPath, gotKey, gotVersion, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey, gotVersion, gotType = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"), r.Header.Get("Content-Type")
		if r.Header.Get("Authorization") != "" {
			t.Errorf("anthropic must not send Authorization, got %q", r.Header.Get("Authorization"))
		}
		body := decodeBody(t, r)
		if body["model"] != "claude-test" {
			t.Errorf("model = %v", body["model"])
		}
		if body["max_tokens"] == nil {
			t.Error("max_tokens is required by the Messages API")
		}
		if body["system"] != "be terse" {
			t.Errorf("system not hoisted: %v", body["system"])
		}
		msgs, _ := body["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("system must not stay in messages, got %d messages", len(msgs))
		}
		if m := msgs[0].(map[string]any); m["role"] != "user" {
			t.Errorf("role = %v", m["role"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]any{{"type": "text", "text": `{"summary":"ok"}`}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 11, "output_tokens": 7},
		})
	}))
	defer srv.Close()

	c := New(Config{AnthropicKey: "sk-ant-test", AnthropicBaseURL: srv.URL, AnthropicModel: "claude-test"})
	if c.Provider() != ProviderAnthropic {
		t.Fatalf("provider = %q", c.Provider())
	}
	out, usage, err := c.Complete(context.Background(), []Message{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "explain"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != `{"summary":"ok"}` {
		t.Fatalf("out = %q", out)
	}
	if usage.PromptTokens != 11 || usage.CompletionTokens != 7 {
		t.Fatalf("usage = %+v", usage)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q", gotPath)
	}
	if gotKey != "sk-ant-test" || gotVersion != AnthropicVersion {
		t.Errorf("headers: key=%q version=%q", gotKey, gotVersion)
	}
	if !strings.HasPrefix(gotType, "application/json") {
		t.Errorf("content-type = %q", gotType)
	}
}

// Non-text blocks (e.g. thinking) must be skipped, not concatenated raw.
func TestAnthropicConcatenatesTextBlocksOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{
				{"type": "thinking", "thinking": "secret"},
				{"type": "text", "text": "{\"a\":"},
				{"type": "text", "text": "1}"},
			},
		})
	}))
	defer srv.Close()
	c := New(Config{AnthropicKey: "k", AnthropicBaseURL: srv.URL})
	out, _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "x"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "{\"a\":1}" {
		t.Fatalf("out = %q (thinking leaked?)", out)
	}
}

// Provider errors must surface the provider's own message, not a bare code.
func TestAnthropicErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "authentication_error", "message": "invalid x-api-key"},
		})
	}))
	defer srv.Close()
	c := New(Config{AnthropicKey: "bad", AnthropicBaseURL: srv.URL})
	_, _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "x"}})
	if err == nil || !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Fatalf("want provider message in error, got %v", err)
	}
}

// The OpenAI path must keep working unchanged (regression guard).
func TestOpenAIWireUnchanged(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "hello"}}},
			"usage":   map[string]int{"prompt_tokens": 3, "completion_tokens": 2},
		})
	}))
	defer srv.Close()
	// The reasoning hint is gated on the host, so a private gateway keeps it
	// and api.openai.com drops it (covered by TestReasoningHintGating).
	c := New(Config{OpenAIKey: "sk-test", OpenAIBaseURL: srv.URL, OpenAIModel: "m"})
	out, usage, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil || out != "hello" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if gotPath != "/chat/completions" || gotAuth != "Bearer sk-test" {
		t.Errorf("path=%q auth=%q", gotPath, gotAuth)
	}
	if usage.PromptTokens != 3 || usage.CompletionTokens != 2 {
		t.Errorf("usage=%+v", usage)
	}
}

// Selection: no keys → none; Anthropic key → anthropic; both → anthropic
// (adding the key is a deliberate switch).
func TestProviderSelection(t *testing.T) {
	if p := New(Config{}).Provider(); p != ProviderNone {
		t.Errorf("no keys: provider = %q", p)
	}
	if p := New(Config{OpenAIKey: "k"}).Provider(); p != ProviderOpenAI {
		t.Errorf("openai key: provider = %q", p)
	}
	if p := New(Config{OpenAIKey: "a", AnthropicKey: "b"}).Provider(); p != ProviderAnthropic {
		t.Errorf("both keys: provider = %q", p)
	}
}

// Defaults must be the public endpoints and a real model id.
func TestDefaults(t *testing.T) {
	c := New(Config{AnthropicKey: "k"})
	if c.BaseURL() != AnthropicBaseURL {
		t.Errorf("base = %q", c.BaseURL())
	}
	if c.Model() != AnthropicModel {
		t.Errorf("model = %q", c.Model())
	}
	if New(Config{}).Enabled() {
		t.Error("no key must not be enabled")
	}
}

// The reasoning hint is a gateway extension: api.openai.com rejects it, so it
// must be sent only to third-party (OpenAI-compatible) hosts.
func TestReasoningHintGating(t *testing.T) {
	cases := []struct {
		base string
		want bool // true = hint must be sent
	}{
		{"https://api.openai.com/v1", false},
		{"https://openai.com", false},
		{"https://catalog.onenexus.cloud/v1", true},
		{"http://127.0.0.1:9999/v1", true},
	}
	for _, tc := range cases {
		c := New(Config{OpenAIKey: "k", OpenAIBaseURL: tc.base})
		got := c.reason != nil
		if got != tc.want {
			t.Errorf("%s: reasoning hint = %v, want %v", tc.base, got, tc.want)
		}
	}
	// Anthropic never takes the OpenAI hint.
	if c := New(Config{AnthropicKey: "k"}); c.reason != nil {
		t.Error("anthropic client must not carry the OpenAI reasoning hint")
	}
}
