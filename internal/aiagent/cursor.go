// Package aiagent — Cursor Cloud Agent engine. Speaks the REST API
// (api.cursor.com /v1/agents) so it works from Go without the TS/Python
// SDKs: one cloud agent per RCA, poll the run to terminal, parse the reply.
// Selected automatically when CursorAPIKey is set — OpenAI-compatible
// endpoints stay the chat engine; template fallback still guards the demo.
package aiagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	cursorDefaultURL   = "https://api.cursor.com"
	cursorPollInterval = 3 * time.Second
	cursorPollTimeout  = 5 * time.Minute
)

// cursorAgentResp is the create-agent response (agent + initial run).
type cursorAgentResp struct {
	Agent struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"agent"`
	Run struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"run"`
}

// cursorRunResp is the get-run response (terminal runs carry result).
type cursorRunResp struct {
	ID     string `json:"id"`
	Status string `json:"status"` // CREATING|RUNNING|FINISHED|ERROR|CANCELLED|EXPIRED
	Result string `json:"result"`
}

// cursorRCA runs the RCA prompt through a Cursor cloud agent and returns the
// final assistant reply. The agent runs against this repo (coldtrace) so it
// can also read code/infra context — the grounded Mongo facts ride in the
// prompt either way.
func (c *Client) cursorRCA(ctx context.Context, prompt string) (string, error) {
	agentID, runID, err := c.cursorCreate(ctx, prompt)
	if err != nil {
		return "", err
	}
	return c.cursorWait(ctx, agentID, runID)
}

// cursorCreate enqueues one cloud agent run with the RCA prompt.
func (c *Client) cursorCreate(ctx context.Context, prompt string) (agentID, runID string, err error) {
	body, _ := json.Marshal(map[string]any{
		"prompt": map[string]any{"text": prompt},
		"model":  map[string]any{"id": c.cfg.CursorModel},
		"repos":  []map[string]any{{"url": c.cfg.CursorRepo, "startingRef": "main"}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.CursorAPIURL+"/v1/agents", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.SetBasicAuth(c.cfg.CursorAPIKey, "") // Cloud Agents API: Basic auth
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("cursor create http %d", resp.StatusCode)
	}
	var ar cursorAgentResp
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return "", "", err
	}
	if ar.Agent.ID == "" || ar.Run.ID == "" {
		return "", "", fmt.Errorf("cursor create: missing agent/run id")
	}
	return ar.Agent.ID, ar.Run.ID, nil
}

// cursorWait polls the run until terminal and returns the final reply text.
func (c *Client) cursorWait(ctx context.Context, agentID, runID string) (string, error) {
	url := fmt.Sprintf("%s/v1/agents/%s/runs/%s", c.cfg.CursorAPIURL, agentID, runID)
	deadline := time.Now().Add(cursorPollTimeout)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", err
		}
		req.SetBasicAuth(c.cfg.CursorAPIKey, "")
		resp, err := c.client.Do(req)
		if err != nil {
			return "", err
		}
		var run cursorRunResp
		derr := json.NewDecoder(resp.Body).Decode(&run)
		resp.Body.Close()
		if derr != nil {
			return "", derr
		}
		switch run.Status {
		case "FINISHED":
			return run.Result, nil
		case "ERROR", "CANCELLED", "EXPIRED":
			return "", fmt.Errorf("cursor run %s: %s", runID, run.Status)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("cursor run %s: poll timeout", runID)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(cursorPollInterval):
		}
	}
}

// parseRCAJSON is the shared strict-JSON parse. Tolerates reasoning-model
// output quirks: illow tags, markdown fences, prose around the JSON object.
func parseRCAJSON(content, model string) (*RCA, error) {
	raw := stripToJSONObject(stripThink(strings.TrimSpace(content)))
	var rca RCA
	if err := json.Unmarshal([]byte(raw), &rca); err != nil {
		return nil, fmt.Errorf("rca json parse: %w", err)
	}
	rca.Model = model
	return &rca, nil
}

// stripThink removes illow...</think reasoning scaffolding and plain
// markdown code fences that reasoning models emit before the answer.
func stripThink(s string) string {
	if i := strings.LastIndex(s, "</think"); i >= 0 {
		s = s[i+len("</think"):]
		return strings.TrimSpace(s)
	}
	if i := strings.LastIndex(s, "illow"); i >= 0 {
		s = s[i+len("illow"):]
		return strings.TrimSpace(s)
	}
	return s
}

// stripToJSONObject extracts the outermost {...} block: after fence/prose
// stripping, the RCA object is the first balanced JSON object in s.
func stripToJSONObject(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return s
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case inStr && c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:]
}

// quote JSON-encodes s for embedding in a test fixture.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// jsonDecode decodes a request body (test helper).
func jsonDecode(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// idOf builds a bson ObjectID hex for tests (real hex passthrough).
func idOf(hex string) bson.ObjectID {
	id, _ := bson.ObjectIDFromHex(hex)
	return id
}
