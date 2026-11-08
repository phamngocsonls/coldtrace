// Package ai — root-cause analysis over a pluggable LLM provider.
//
// Provider selection is delegated to internal/llmwire: an Anthropic key wins
// over an OpenAI-compatible one, so operators can switch engines with an env
// var and no code change. Keys arrive via env and are never logged or
// persisted outside the process.
package ai

import (
	"context"
	"time"

	"github.com/onemount/coldtrace/internal/llmwire"
)

// LLMConfig configures the RCA client. The two provider blocks are mutually
// exclusive in practice: whichever key is set selects the engine.
type LLMConfig struct {
	// OpenAI-compatible endpoint (api.openai.com or a gateway such as the
	// OneNexus catalog).
	BaseURL string
	APIKey  string
	Model   string

	// Anthropic Messages API.
	AnthropicBaseURL string
	AnthropicAPIKey  string
	AnthropicModel   string

	Timeout time.Duration
}

// LLM is the RCA client (a thin, alias-friendly wrapper over llmwire).
type LLM struct {
	cfg LLMConfig
	c   *llmwire.Client
}

// NewLLM builds a client; zero-value Timeout falls back to 30s.
func NewLLM(cfg LLMConfig) *LLM {
	return &LLM{cfg: cfg, c: llmwire.New(llmwire.Config{
		OpenAIBaseURL:    cfg.BaseURL,
		OpenAIKey:        cfg.APIKey,
		OpenAIModel:      cfg.Model,
		AnthropicBaseURL: cfg.AnthropicBaseURL,
		AnthropicKey:     cfg.AnthropicAPIKey,
		AnthropicModel:   cfg.AnthropicModel,
		Timeout:          cfg.Timeout,
	})}
}

// Enabled reports whether a provider is configured (otherwise callers skip LLM
// features instead of firing unauthenticated requests).
func (l *LLM) Enabled() bool { return l != nil && l.c.Enabled() }

// Provider reports the active wire format: "anthropic" | "openai" | "".
func (l *LLM) Provider() string {
	if l == nil {
		return ""
	}
	return string(l.c.Provider())
}

// BaseURL reports the active provider's endpoint (for startup logs).
func (l *LLM) BaseURL() string {
	if l == nil {
		return ""
	}
	return l.c.BaseURL()
}

// Model reports the configured model id (for logging/audit fields).
func (l *LLM) Model() string {
	if l == nil {
		return ""
	}
	return l.c.Model()
}

// ChatMessage is one chat message.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Usage reports token counts (zero-valued when the provider omits them).
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// ChatComplete sends one completion and returns the assistant content plus
// token usage. usage is nil on error.
func (l *LLM) ChatComplete(ctx context.Context, msgs []ChatMessage) (string, *Usage, error) {
	wire := make([]llmwire.Message, len(msgs))
	for i, m := range msgs {
		wire[i] = llmwire.Message{Role: m.Role, Content: m.Content}
	}
	out, usage, err := l.c.Complete(ctx, wire)
	if err != nil {
		return "", nil, err
	}
	u := &Usage{PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens}
	return out, u, nil
}
