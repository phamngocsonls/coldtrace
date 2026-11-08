// Package aiagent — GroundedChat: the agentic chat loop for the Ops Console
// AI assistant. The model may call read-only MongoDB tools (fleet, device,
// incidents, telemetry stats) for a few rounds, then streams the final
// answer. Actions are always suggestions only — the agent NEVER executes;
// humans run them from the console.
//
// Fallbacks: no key → word-echo degraded chat; tool/backend failure → the
// error rides back to the console on the SSE stream; the demo never stalls.
package aiagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

const (
	maxToolRounds   = 3 // model may call tools at most this many times
	groundedTimeout = 110 * time.Second
	toolResultMax   = 8 << 10 // truncate oversized tool results
)

// chatMessageFull is the wire message with optional tool fields.
type chatMessageFull struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []toolCallWire `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type toolCallWire struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type toolCallChunk struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatRequestFull struct {
	Model      string            `json:"model"`
	Messages   []chatMessageFull `json:"messages"`
	Tools      []toolDef         `json:"tools,omitempty"`
	ToolChoice any               `json:"tool_choice,omitempty"`
	Stream     bool              `json:"stream,omitempty"`
	Reasoning  map[string]any    `json:"reasoning,omitempty"`
}

type chatChunkFull struct {
	Choices []struct {
		Delta struct {
			Content   string          `json:"content"`
			ToolCalls []toolCallChunk `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

const chatSystemPrompt = "You are the ColdTrace AI agent for an industrial " +
	"cold-chain freezer fleet. You answer operator questions about devices, " +
	"incidents and temperature telemetry. Ground every factual claim in tool " +
	"results before answering; never invent numbers. You SUGGEST actions " +
	"from this allowlist only: compressor_restart, defrost, reboot, " +
	"set_setpoint, none — and a human always executes them. Keep answers " +
	"short, factual and actionable."

// GroundedChat runs the agentic chat loop and streams answer tokens to
// onDelta. Tool rounds are silent (facts are fetched server-side); any
// narration the model writes alongside tool calls is streamed too, and the
// final assistant text is delivered when the model stops calling tools.
func (c *Client) GroundedChat(ctx context.Context, db *mongo.Database, msgs []ChatMessage, onDelta func(string)) error {
	ctx, cancel := context.WithTimeout(ctx, groundedTimeout)
	defer cancel()
	if !c.Enabled() || db == nil {
		return c.ChatStream(ctx, msgs, onDelta) // degraded path echoes words
	}

	// convert the public (role/content) history into full wire messages
	history := make([]chatMessageFull, 0, len(msgs)+2)
	history = append(history, chatMessageFull{Role: "system", Content: chatSystemPrompt})
	for _, m := range msgs {
		history = append(history, chatMessageFull{Role: m.Role, Content: m.Content})
	}

	answered := false
	for round := 0; round < maxToolRounds; round++ {
		assistantMsg, err := c.groundedTurn(ctx, history)
		if err != nil {
			return err
		}
		// Surface any narration the model wrote even when it also asked for
		// tools, so nothing the operator should see gets silently dropped.
		if assistantMsg.Content != "" {
			onDelta(assistantMsg.Content)
			answered = true
		}
		if len(assistantMsg.ToolCalls) == 0 {
			return nil
		}
		// record the assistant tool-call message, execute tools, append results
		history = append(history, *assistantMsg)
		for _, tc := range assistantMsg.ToolCalls {
			out, terr := runTool(ctx, db, tc.Function.Name, tc.Function.Arguments)
			if terr != nil {
				out = `{"error":` + mustJSON(terr.Error()) + `}`
			}
			if len(out) > toolResultMax {
				out = truncateRunes(out, toolResultMax)
			}
			history = append(history, chatMessageFull{
				Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: out,
			})
		}
	}
	// maxToolRounds turns used and the model still wants tools — surface the
	// narration we already streamed and stop; never silently return nothing.
	if answered {
		return nil
	}
	return fmt.Errorf("ai agent used all %d tool rounds without a final answer", maxToolRounds)
}

// groundedTurn does one chat completion (non-streaming) that may request
// tool calls; plain text deltas are forwarded to onDelta as they would be
// in the final answer so single-turn questions stream too.
func (c *Client) groundedTurn(ctx context.Context, history []chatMessageFull) (*chatMessageFull, error) {
	if err := c.requireStreamingEngine(); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(chatRequestFull{
		Model:     c.cfg.Model,
		Messages:  history,
		Tools:     agentTools,
		Stream:    false,
		Reasoning: c.reasoningIfConfigured(),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var eb struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := fmt.Sprintf("llm http %d", resp.StatusCode)
		if derr := json.NewDecoder(resp.Body).Decode(&eb); derr == nil && eb.Error.Message != "" {
			msg += ": " + eb.Error.Message
		}
		return nil, fmt.Errorf("%s", msg)
	}
	var cr struct {
		Choices []struct {
			Message struct {
				Content   string         `json:"content"`
				ToolCalls []toolCallWire `json:"tool_calls,omitempty"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, err
	}
	if cr.Error != nil && cr.Error.Message != "" {
		return nil, fmt.Errorf("llm error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("llm returned no choices")
	}
	m := cr.Choices[0].Message
	out := &chatMessageFull{Role: "assistant", Content: m.Content, ToolCalls: m.ToolCalls}
	return out, nil
}
