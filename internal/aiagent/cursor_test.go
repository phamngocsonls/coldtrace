// Tests for the RCA response parser: reasoning-model scaffolding, markdown
// fences, and prose around the JSON object must all parse.
package aiagent

import (
	"strings"
	"testing"
)

const goodJSON = `{"summary":"s","rootCause":"r","evidence":["e"],"actions":["none"]}`

func TestParseRCAJSONClean(t *testing.T) {
	rca, err := parseRCAJSON(goodJSON, "m")
	if err != nil {
		t.Fatalf("clean parse failed: %v", err)
	}
	if rca.Summary != "s" || rca.RootCause != "r" {
		t.Fatalf("fields lost: %+v", rca)
	}
	if rca.Model != "m" {
		t.Fatalf("model not stamped: %+v", rca)
	}
}

func TestParseRCAJSONThinkTag(t *testing.T) {
	in := "illowthe user asks for an RCA. Let me analyze.illow\n" + goodJSON
	rca, err := parseRCAJSON(in, "m")
	if err != nil {
		t.Fatalf("think-wrapped parse failed: %v", err)
	}
	if rca.Summary != "s" {
		t.Fatalf("summary = %q", rca.Summary)
	}
}

func TestParseRCAJSONFenced(t *testing.T) {
	in := "```json\n" + goodJSON + "\n```"
	rca, err := parseRCAJSON(in, "m")
	if err != nil {
		t.Fatalf("fenced parse failed: %v", err)
	}
	if rca.RootCause != "r" {
		t.Fatalf("rootCause = %q", rca.RootCause)
	}
}

func TestParseRCAJSONProseAroundObject(t *testing.T) {
	in := "Here is my analysis:\n\n" + goodJSON + "\n\nLet me know if you need more."
	rca, err := parseRCAJSON(in, "m")
	if err != nil {
		t.Fatalf("prose-wrapped parse failed: %v", err)
	}
	if rca.Summary != "s" {
		t.Fatalf("summary = %q", rca.Summary)
	}
}

func TestParseRCAJSONNestedBraces(t *testing.T) {
	// braces inside strings and nested objects must not confuse the extractor
	in := `{"summary":"has } brace","rootCause":"r","evidence":[],"actions":["set_setpoint"]}`
	rca, err := parseRCAJSON(in, "m")
	if err != nil {
		t.Fatalf("nested-brace parse failed: %v", err)
	}
	if !strings.Contains(rca.Summary, "}") {
		t.Fatalf("string content corrupted: %q", rca.Summary)
	}
}

func TestParseRCAJSONGarbageFails(t *testing.T) {
	if _, err := parseRCAJSON("not json at all", "m"); err == nil {
		t.Fatalf("garbage must not parse")
	}
}

func TestStripToJSONObjectFirstObjectWins(t *testing.T) {
	in := `prefix {"a":1} suffix {"b":2}`
	if got := stripToJSONObject(in); got != `{"a":1}` {
		t.Fatalf("stripToJSONObject = %q, want the first object", got)
	}
}
