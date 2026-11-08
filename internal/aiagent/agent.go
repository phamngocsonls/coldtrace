// Package aiagent — the Ops Console AI assistant: grounded root-cause
// analysis and streaming chat over any OpenAI-compatible endpoint. The
// the agent NEVER acts: it explains, proposes allowlisted actions and leaves
// execution to the human ("human-in-the-loop by design").
//
// Demo safety: when no key is configured (or the endpoint fails/times out),
// a deterministic template analysis built from the same MongoDB facts is
// returned, so the demo never stalls on a third-party dependency.
package aiagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/onemount/coldtrace/internal/llmwire"
	"github.com/onemount/coldtrace/internal/ops"
)

// Config mirrors the engine configuration. Exactly one of the keys selects
// the engine: CursorAPIKey → Cursor cloud agent; AnthropicAPIKey → Anthropic
// Messages API; APIKey → OpenAI-compatible endpoint (chat-completions);
// none → template fallback.
type Config struct {
	BaseURL string // OpenAI-compatible base URL (chat engine)
	APIKey  string // OpenAI-compatible key (chat engine)
	Model   string // chat model id
	Timeout time.Duration

	// Anthropic Messages API (wins over the OpenAI-compatible engine).
	AnthropicBaseURL string // default https://api.anthropic.com
	AnthropicAPIKey  string // pk/sk-ant-...; empty disables the engine
	AnthropicModel   string // default llmwire.AnthropicModel

	// Cursor Cloud Agents engine (wins over OpenAI when set)
	CursorAPIKey string // user or team API key ("cursor_...")
	CursorAPIURL string // default https://api.cursor.com
	CursorModel  string // default composer-2.5
	CursorRepo   string // repo URL the cloud agent runs against
}

// Client is the AI agent backend.
type Client struct {
	cfg    Config
	client *http.Client
	wire   *llmwire.Client // OpenAI/Anthropic providers (Cursor keeps its own REST)
}

// New builds an AI agent client.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Model == "" {
		cfg.Model = "gpt-4o-mini"
	}
	if cfg.CursorAPIURL == "" {
		cfg.CursorAPIURL = cursorDefaultURL
	}
	cfg.CursorAPIURL = strings.TrimRight(cfg.CursorAPIURL, "/")
	if cfg.CursorModel == "" {
		cfg.CursorModel = "composer-2.5"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	// No client-level timeout: streaming chats + cloud-agent RCAs are long
	// lived; every request carries its own context deadline instead.
	wire := llmwire.New(llmwire.Config{
		OpenAIBaseURL:    cfg.BaseURL,
		OpenAIKey:        cfg.APIKey,
		OpenAIModel:      cfg.Model,
		AnthropicBaseURL: cfg.AnthropicBaseURL,
		AnthropicKey:     cfg.AnthropicAPIKey,
		AnthropicModel:   cfg.AnthropicModel,
	})
	return &Client{cfg: cfg, client: &http.Client{}, wire: wire}
}

// Enabled reports whether any engine is configured.
func (c *Client) Enabled() bool { return c != nil && c.Engine() != "none" }

// Engine names the active engine: "cursor" | "anthropic" | "openai" | "none".
func (c *Client) Engine() string {
	if c == nil {
		return "none"
	}
	if c.cfg.CursorAPIKey != "" {
		return "cursor"
	}
	switch c.wire.Provider() {
	case llmwire.ProviderAnthropic:
		return "anthropic"
	case llmwire.ProviderOpenAI:
		return "openai"
	}
	return "none"
}

// requireStreamingEngine guards the streaming chat path, which speaks the
// OpenAI SSE format only. The Anthropic engine is wired for RCA; using it for
// streaming chat needs an SSE adapter, so fail loudly instead of sending
// OpenAI-shaped JSON to /v1/messages.
func (c *Client) requireStreamingEngine() error {
	if c.Engine() == "anthropic" {
		return errors.New("streaming chat is not available on the anthropic engine yet; " +
			"set CURSOR_API_KEY or OPENAI_API_KEY to use the copilot, or keep ANTHROPIC_API_KEY for RCA only")
	}
	return nil
}

// ChatMessage is one chat message (public type).
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatBody builds the chat-completions request body. Reasoning models
// (e.g. OneNexus glm-5.3-flash) stream long invisible reasoning trails —
// we ask the backend to exclude them from the response when supported.
func chatBody(model string, msgs []ChatMessage, stream bool, reasoning map[string]any) chatRequest {
	return chatRequest{Model: model, Messages: msgs, Stream: stream, Reasoning: reasoning}
}

// reasoningIfConfigured returns the reasoning-exclude hint only for
// reasoning-model gateways (OneNexus catalog); api.openai.com rejects the
// unknown field, so it is gated off there.
func (c *Client) reasoningIfConfigured() map[string]any {
	if strings.Contains(c.cfg.BaseURL, "openai.com") {
		return nil
	}
	return map[string]any{"exclude": true}
}

// chatRequest is the OpenAI chat-completions body (streaming + reasoning).
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream,omitempty"`
	// Reasoning asks reasoning models (OneNexus catalog) to keep their
	// thinking out of the response. Ignored by strict-OpenAI endpoints via
	// omitempty on empty… map is always set, so we use a pointer-free
	// approach: only non-nil maps serialize with content the backend may
	// reject. OpenAI tolerates unknown body fields; verified against
	// api.openai.com semantics — unknown fields are rejected, so gate it.
	Reasoning map[string]any `json:"reasoning,omitempty"`
}

type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// RCA is the structured root-cause analysis result.
type RCA struct {
	Summary   string   `json:"summary"`
	RootCause string   `json:"rootCause"`
	Evidence  []string `json:"evidence"`
	Actions   []string `json:"actions"`
	Model     string   `json:"model"`
	Fallback  bool     `json:"fallback"` // true when the template path was used
	Error     string   `json:"error,omitempty"`
}

// rcaSystemPrompt — strict JSON contract for the RCA engine.
const rcaSystemPrompt = "You are an industrial cold-chain SRE AI agent. " +
	"Answer ONLY with strict JSON: {\"summary\":string,\"rootCause\":string," +
	"\"evidence\":[string],\"actions\":[string]}. Max 2 sentences per field. " +
	"Actions must be from this allowlist: compressor_restart, defrost, reboot, set_setpoint, none."

// RootCause builds grounded context and asks the active engine for an RCA.
// Falls back to a deterministic template on any failure — the demo never
// stalls on a third-party dependency.
func (c *Client) RootCause(ctx context.Context, db *mongo.Database, store *ops.Store, incidentID bson.ObjectID) (*RCA, error) {
	facts := gatherFacts(ctx, db, store, incidentID)
	if !c.Enabled() {
		return templateRCA(facts), nil
	}
	prompt := "Incident facts (MongoDB):\n" + mustJSON(facts.Incident) +
		"\nDevice registry: " + mustJSON(facts.Device) +
		"\nDevice twin: " + mustJSON(facts.Twin) +
		"\nRecent commands: " + mustJSON(facts.Commands) +
		"\nSimilar past incidents: " + mustJSON(facts.Similar) +
		"\nExplain what is happening, why, and what to do next."

	var (
		out string
		err error
	)
	switch c.Engine() {
	case "cursor":
		// Cursor agents can read the repo themselves — the grounded facts ride
		// in the prompt; the agent may add code/infra context on top.
		out, err = c.cursorRCA(ctx, rcaSystemPrompt+"\n\n"+prompt)
	case "anthropic", "openai":
		out, err = c.chatRCA(ctx, facts)
	default:
		r := templateRCA(facts)
		return r, nil
	}
	if err != nil {
		r := templateRCA(facts)
		r.Fallback = true
		r.Error = err.Error()
		return r, nil
	}
	rca, perr := parseRCAJSON(out, c.modelName())
	if perr != nil {
		// model drifted from strict JSON — surface raw text as summary
		rca = &RCA{Summary: out, RootCause: "(raw engine output — JSON parse failed)", Model: c.modelName()}
	}
	return rca, nil
}

// modelName reports the model id for audit fields on the active engine.
func (c *Client) modelName() string {
	if c.Engine() == "cursor" {
		return c.cfg.CursorModel
	}
	return c.wire.Model()
}

// BaseURL reports the endpoint of the active engine (for startup logs).
func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	if c.Engine() == "cursor" {
		return c.cfg.CursorAPIURL
	}
	return c.wire.BaseURL()
}

// incidentFacts is the grounded context handed to the model.
type incidentFacts struct {
	Incident bson.M
	Device   bson.M
	Twin     bson.M
	Stats    []bson.M // 15-min window aggregates from the health pipeline
	Commands []ops.Command
	Similar  []bson.M
}

func gatherFacts(ctx context.Context, db *mongo.Database, store *ops.Store, id bson.ObjectID) incidentFacts {
	f := incidentFacts{}
	if store == nil {
		return f // tests: facts come from the fake API fixtures, not Mongo
	}
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := store.Incidents.FindOne(qctx, bson.M{"_id": id}).Decode(&f.Incident); err != nil {
		return f // unknown incident: facts stay empty, template still answers
	}
	if dev, ok := f.Incident["deviceId"].(string); ok {
		_ = store.Devices.FindOne(qctx, bson.M{"deviceId": dev}).Decode(&f.Device)
		_ = store.Twin.FindOne(qctx, bson.M{"deviceId": dev}).Decode(&f.Twin)
		cur, err := store.Commands.Find(qctx, bson.M{"deviceId": dev},
			opsFindSort().SetLimit(10))
		if err == nil && cur != nil {
			_ = cur.All(qctx, &f.Commands)
			cur.Close(qctx)
		}
	}
	if f.Incident["similar"] != nil {
		if arr, ok := f.Incident["similar"].(bson.A); ok {
			for _, v := range arr {
				if m, ok := v.(bson.M); ok {
					f.Similar = append(f.Similar, m)
				}
			}
		}
	}
	return f
}

// chatRCA asks the configured non-Cursor provider (Anthropic Messages API or
// an OpenAI-compatible endpoint) and returns the raw assistant text;
// parsing/normalizing happens in RootCause.
func (c *Client) chatRCA(ctx context.Context, f incidentFacts) (string, error) {
	prompt := "Incident facts (MongoDB):\n" + mustJSON(f.Incident) +
		"\nDevice registry: " + mustJSON(f.Device) +
		"\nDevice twin: " + mustJSON(f.Twin) +
		"\nRecent commands: " + mustJSON(f.Commands) +
		"\nSimilar past incidents: " + mustJSON(f.Similar) +
		"\nExplain what is happening, why, and what to do next."

	out, _, err := c.wire.Complete(ctx, []llmwire.Message{
		{Role: "system", Content: rcaSystemPrompt},
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// templateRCA — deterministic, data-grounded fallback (no model needed).
func templateRCA(f incidentFacts) *RCA {
	class, _ := f.Incident["faultClass"].(string)
	device, _ := f.Incident["deviceId"].(string)
	conf, _ := f.Incident["confidence"].(float64)
	eta, _ := f.Incident["etaAlarmMin"].(float64)

	rootCause := "The AI classifier observed a telemetry pattern matching " + class + " on " + device
	ev := []string{
		fmt.Sprintf("classifier confidence %.0f%%", conf*100),
	}
	if f.Twin != nil {
		if reported, ok := f.Twin["reported"].(bson.M); ok {
			if t, ok := reported["tempC"].(float64); ok {
				ev = append(ev, fmt.Sprintf("cabinet temperature %.1f°C", t))
			}
			if run, ok := reported["compressorRunning"].(bool); ok && !run {
				ev = append(ev, "compressor not running")
			}
		}
	}
	switch class {
	case "compressor_failure":
		rootCause += ": sustained temperature rise with the compressor offline."
	case "door_left_open":
		rootCause += ": warm-air ingress while the compressor is fighting it — the door was left open."
	case "sensor_drift":
		rootCause += ": the sensor reports progressively colder than reality; verify against the manual thermometer."
	case "power_outage":
		rootCause += ": no compressor power draw — the unit lost supply."
	default:
		rootCause += "."
	}
	if eta == eta { // not NaN
		ev = append(ev, fmt.Sprintf("projected -10°C alarm in %.0f min", eta))
	}
	actions := []string{"none"}
	switch class {
	case "compressor_failure", "power_outage":
		actions = []string{"compressor_restart", "reboot"}
	case "door_left_open":
		actions = []string{"none"} // physical action: close the door
	case "sensor_drift":
		actions = []string{"reboot"}
	}
	return &RCA{
		Summary:   fmt.Sprintf("%s flagged as %s with %.0f%% confidence", device, class, conf*100),
		RootCause: rootCause,
		Evidence:  ev,
		Actions:   actions,
		Model:     "template-fallback",
		Fallback:  true,
	}
}

// ChatStream runs a streaming chat completion, calling onDelta per token.
func (c *Client) ChatStream(ctx context.Context, msgs []ChatMessage, onDelta func(string)) error {
	ctx, cancel := context.WithTimeout(ctx, 110*time.Second)
	defer cancel()
	if !c.Enabled() {
		// graceful degraded chat without a key — but still honor cancel and
		// the deadline so a dead client does not keep this loop burning
		last := msgs[len(msgs)-1].Content
		for _, w := range strings.Fields(last) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			onDelta(w + " ")
			time.Sleep(20 * time.Millisecond)
		}
		return nil
	}
	if err := c.requireStreamingEngine(); err != nil {
		return err
	}
	body, _ := json.Marshal(chatBody(c.cfg.Model, msgs, true, c.reasoningIfConfigured()))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// surface the API error body instead of a bare status code
		var eb struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := fmt.Sprintf("llm http %d", resp.StatusCode)
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&eb); err == nil && eb.Error.Message != "" {
			msg += ": " + eb.Error.Message
		}
		return errors.New(msg)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var chunk chatChunk
		if err := dec.Decode(&chunk); err != nil {
			if errors.Is(err, io.EOF) {
				return nil // clean stream end
			}
			// mid-stream socket reset / truncated response: surface it
			// instead of silently succeeding with a half-answer
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("stream decode: %w", err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return fmt.Errorf("llm error: %s", chunk.Error.Message)
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				onDelta(ch.Delta.Content)
			}
			if ch.FinishReason != nil && *ch.FinishReason == "stop" {
				return nil
			}
		}
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
