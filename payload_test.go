package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestShrinkPayloadNoop(t *testing.T) {
	raw := []byte(`{"type":"AgentSessionEvent","action":"created","agentSession":{"id":"abc"}}`)
	got := shrinkPayload(raw)
	if string(got) != string(raw) {
		t.Fatalf("expected payload under the limit to pass through unchanged, got %q", got)
	}
}

func TestShrinkPayloadTopLevelField(t *testing.T) {
	big := strings.Repeat("x", 20*1024)
	raw, err := json.Marshal(map[string]any{
		"type":          "AgentSessionEvent",
		"action":        "created",
		"promptContext": "keep-the-tail:" + big,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= maxDispatchPayload {
		t.Fatalf("test fixture should exceed the limit, got %d bytes", len(raw))
	}

	got := shrinkPayload(raw)
	if len(got) > maxDispatchPayload {
		t.Fatalf("shrunk payload still exceeds limit: %d > %d", len(got), maxDispatchPayload)
	}

	var out map[string]any
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("shrunk payload isn't valid JSON: %v", err)
	}
	if out["type"] != "AgentSessionEvent" {
		t.Fatalf("expected non-truncated small fields to survive, got %v", out["type"])
	}
	pc, _ := out["promptContext"].(string)
	if !strings.Contains(pc, truncatedMarker) {
		t.Fatalf("expected truncation marker in shrunk field, got len %d", len(pc))
	}
	if !strings.HasSuffix(pc, "xxxx") {
		t.Fatalf("expected the tail of the original string to survive, got suffix %q", pc[max(0, len(pc)-20):])
	}
}

func TestShrinkPayloadNestedField(t *testing.T) {
	big := strings.Repeat("y", 20*1024)
	raw, err := json.Marshal(map[string]any{
		"type":   "AgentSessionEvent",
		"action": "created",
		"agentSession": map[string]any{
			"id":            "abc",
			"promptContext": big,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := shrinkPayload(raw)
	if len(got) > maxDispatchPayload {
		t.Fatalf("shrunk payload still exceeds limit: %d > %d", len(got), maxDispatchPayload)
	}

	var out struct {
		AgentSession struct {
			ID            string `json:"id"`
			PromptContext string `json:"promptContext"`
		} `json:"agentSession"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("shrunk payload isn't valid JSON: %v", err)
	}
	if out.AgentSession.ID != "abc" {
		t.Fatalf("expected nested non-truncated fields to survive, got %q", out.AgentSession.ID)
	}
	if !strings.Contains(out.AgentSession.PromptContext, truncatedMarker) {
		t.Fatal("expected truncation marker in shrunk nested field")
	}
}

func TestShrinkPayloadNonObjectFallsBack(t *testing.T) {
	raw := []byte(`"` + strings.Repeat("z", 20*1024) + `"`)
	got := shrinkPayload(raw)
	if len(got) > maxDispatchPayload {
		t.Fatalf("fallback payload still exceeds limit: %d > %d", len(got), maxDispatchPayload)
	}
	var out map[string]string
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("fallback payload isn't valid JSON: %v", err)
	}
	if _, ok := out["promptContext"]; !ok {
		t.Fatal("expected fallback envelope to carry a promptContext field")
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
