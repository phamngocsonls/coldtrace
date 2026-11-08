// Package llmwire — one client, two chat wire formats.
//
// ColdTrace talks to LLM providers through one of two request/response shapes:
//
//   - "openai":    POST {base}/chat/completions — system prompt as a message,
//     reply at choices[].message.content, auth via Authorization: Bearer.
//   - "anthropic": POST {base}/messages — system prompt in a top-level field,
//     reply as content[] blocks, auth via x-api-key + anthropic-version.
//
// Both are stateless single-turn passes used for root-cause analysis, so the
// client stays tiny: no streaming, no tool use, no SDK dependency.
package llmwire

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Provider names the wire format.
type Provider string

const (
	ProviderOpenAI    Provider = "openai"
	ProviderAnthropic Provider = "anthropic"
	ProviderNone      Provider = ""
)

// Anthropic defaults; overridable through Config.
const (
	AnthropicBaseURL = "https://api.anthropic.com"
	AnthropicVersion = "2023-06-01"
	AnthropicModel   = "claude-sonnet-4-6"
	// AnthropicMaxTokens bounds the reply. RCA answers are short JSON; this
	// keeps latency and spend predictable.
	AnthropicMaxTokens = 1024
)

// OpenAIBaseURL is the default endpoint when a key is set without a base URL.
const OpenAIBaseURL = "https://api.openai.com/v1"

// Config selects and configures the active provider. Exactly one key selects
// the provider; with neither key the client reports ProviderNone.
type Config struct {
	// OpenAI-compatible (also covers gateways such as the OneNexus catalog).
	OpenAIBaseURL string
	OpenAIKey     string
	OpenAIModel   string

	// Anthropic Messages API.
	AnthropicBaseURL string
	AnthropicKey     string
	AnthropicModel   string

	Timeout time.Duration
}

// Client is a provider-agnostic single-turn chat client.
type Client struct {
	cfg    Config
	http   *http.Client
	reason map[string]any // provider-specific extra body fields, if any
}

// Message is one chat message.
type Message struct {
	Role    string `json:"role"` // "system" | "user" | "assistant"
	Content string `json:"content"`
}

// Usage reports token counts; fields are 0 when the provider omits them.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// New applies defaults and returns a client. Provider selection is resolved by
// Provider(); the Anthropic key wins when both are set, so an operator that
// adds the key deliberately changes engines.
func New(cfg Config) *Client {
	if cfg.OpenAIBaseURL == "" {
		cfg.OpenAIBaseURL = OpenAIBaseURL
	}
	cfg.OpenAIBaseURL = strings.TrimRight(cfg.OpenAIBaseURL, "/")
	if cfg.OpenAIModel == "" {
		cfg.OpenAIModel = "gpt-4o-mini"
	}
	if cfg.AnthropicBaseURL == "" {
		cfg.AnthropicBaseURL = AnthropicBaseURL
	}
	cfg.AnthropicBaseURL = strings.TrimRight(cfg.AnthropicBaseURL, "/")
	if cfg.AnthropicModel == "" {
		cfg.AnthropicModel = AnthropicModel
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	c := &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}
	// Reasoning gateways (OneNexus catalog, not OpenAI proper) accept a
	// directive to keep hidden thinking out of the reply. api.openai.com
	// rejects the unknown field, so it is gated on the host.
	if c.Provider() == ProviderOpenAI && !isFirstPartyOpenAI(cfg.OpenAIBaseURL) {
		c.reason = map[string]any{"exclude": true}
	}
	return c
}

// isFirstPartyOpenAI reports whether the base URL points at OpenAI itself
// (which rejects the non-standard "reasoning" field).
func isFirstPartyOpenAI(base string) bool {
	host := base
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	host = strings.TrimSuffix(host, "/")
	if i := strings.IndexAny(host, "/?"); i >= 0 {
		host = host[:i]
	}
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host == "openai.com" || strings.HasSuffix(host, ".openai.com")
}

// Provider reports the active wire format ("" when no key is configured).
func (c *Client) Provider() Provider {
	if c == nil {
		return ProviderNone
	}
	if c.cfg.AnthropicKey != "" {
		return ProviderAnthropic
	}
	if c.cfg.OpenAIKey != "" {
		return ProviderOpenAI
	}
	return ProviderNone
}

// Enabled reports whether a provider is configured.
func (c *Client) Enabled() bool { return c.Provider() != ProviderNone }

// Model reports the model id of the active provider (for audit fields).
func (c *Client) Model() string {
	if c == nil {
		return ""
	}
	switch c.Provider() {
	case ProviderAnthropic:
		return c.cfg.AnthropicModel
	case ProviderOpenAI:
		return c.cfg.OpenAIModel
	}
	return ""
}

// BaseURL reports the endpoint of the active provider.
func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	if c.Provider() == ProviderAnthropic {
		return c.cfg.AnthropicBaseURL
	}
	return c.cfg.OpenAIBaseURL
}

// Complete sends the messages and returns the assistant text plus usage.
func (c *Client) Complete(ctx context.Context, msgs []Message) (string, *Usage, error) {
	if !c.Enabled() {
		return "", nil, fmt.Errorf("llm not configured (missing API key)")
	}
	if c.Provider() == ProviderAnthropic {
		return c.completeAnthropic(ctx, msgs)
	}
	return c.completeOpenAI(ctx, msgs)
}

// ---------------------------------------------------------------- OpenAI ----

type openAIRequest struct {
	Model     string         `json:"model"`
	Messages  []Message      `json:"messages"`
	Reasoning map[string]any `json:"reasoning,omitempty"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) completeOpenAI(ctx context.Context, msgs []Message) (string, *Usage, error) {
	body, err := json.Marshal(openAIRequest{Model: c.cfg.OpenAIModel, Messages: msgs, Reasoning: c.reason})
	if err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.OpenAIBaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.OpenAIKey)

	var out openAIResponse
	if err := c.do(req, &out); err != nil {
		return "", nil, err
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", nil, fmt.Errorf("llm error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 || out.Choices[0].Message.Content == "" {
		return "", nil, fmt.Errorf("llm returned empty content")
	}
	return out.Choices[0].Message.Content,
		&Usage{PromptTokens: out.Usage.PromptTokens, CompletionTokens: out.Usage.CompletionTokens}, nil
}

// ------------------------------------------------------------- Anthropic ----

// anthropicBlock is one content block. Only text is requested; other block
// types (e.g. thinking) are ignored on decode.
type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type anthropicRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
}

type anthropicResponse struct {
	Content []anthropicBlock `json:"content"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	StopReason string `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// completeAnthropic maps the shared message list onto the Messages API:
// system messages are hoisted into the top-level field (the API rejects the
// "system" role inside messages), and everything else keeps its order.
func (c *Client) completeAnthropic(ctx context.Context, msgs []Message) (string, *Usage, error) {
	var system []string
	conv := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" {
			if s := strings.TrimSpace(m.Content); s != "" {
				system = append(system, s)
			}
			continue
		}
		conv = append(conv, m)
	}
	if len(conv) == 0 {
		return "", nil, fmt.Errorf("anthropic: no user message")
	}

	body, err := json.Marshal(anthropicRequest{
		Model:     c.cfg.AnthropicModel,
		MaxTokens: AnthropicMaxTokens,
		System:    strings.Join(system, "\n\n"),
		Messages:  conv,
	})
	if err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.AnthropicBaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.cfg.AnthropicKey)
	req.Header.Set("anthropic-version", AnthropicVersion)

	var out anthropicResponse
	if err := c.do(req, &out); err != nil {
		return "", nil, err
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", nil, fmt.Errorf("anthropic error: %s", out.Error.Message)
	}
	var sb strings.Builder
	for _, b := range out.Content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return "", nil, fmt.Errorf("anthropic returned empty content")
	}
	return text, &Usage{PromptTokens: out.Usage.InputTokens, CompletionTokens: out.Usage.OutputTokens}, nil
}

// ------------------------------------------------------------------ core ----

// do executes req, maps non-200s to an error that includes the provider's
// message when it sends one, and decodes the body into out.
func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read llm response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Decode first: both providers put the reason in the body.
		if err := json.Unmarshal(raw, out); err == nil {
			if msg := errorMessage(out); msg != "" {
				return fmt.Errorf("llm http %d: %s", resp.StatusCode, msg)
			}
		}
		return fmt.Errorf("llm http %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode llm response: %w", err)
	}
	return nil
}

// errorMessage digs the provider error text out of either response shape.
func errorMessage(v any) string {
	switch r := v.(type) {
	case *openAIResponse:
		if r.Error != nil {
			return r.Error.Message
		}
	case *anthropicResponse:
		if r.Error != nil {
			return r.Error.Message
		}
	}
	return ""
}
