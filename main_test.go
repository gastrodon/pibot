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

func TestParseDirective(t *testing.T) {
	cases := []struct {
		name string
		body string
		want map[string]string
	}{
		{"none", "just fix the bug please", nil},
		{"model only", "do the thing\npibot: model=ollama/qwen2.5-coder:7b", map[string]string{"model": "ollama/qwen2.5-coder:7b"}},
		{"model and thinking", "pibot: model=ollama/qwen2.5-coder:7b thinking=off", map[string]string{"model": "ollama/qwen2.5-coder:7b", "thinking": "off"}},
		{"trailing whitespace tolerated", "pibot: model=anthropic/claude-sonnet-5  \n\n", map[string]string{"model": "anthropic/claude-sonnet-5"}},
		{"case insensitive prefix", "PIBOT: thinking=high", map[string]string{"thinking": "high"}},
		{"not the last line is ignored", "pibot: model=ollama/x\nbut then I kept typing", nil},
		{"empty body", "", nil},
		{"mentions pibot mid-prose is not a directive", "hey pibot: model=x this is not anchored", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDirective(tc.body)
			if len(got) != len(tc.want) {
				t.Fatalf("parseDirective(%q) = %v, want %v", tc.body, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("parseDirective(%q)[%q] = %q, want %q", tc.body, k, got[k], v)
				}
			}
		})
	}
}

func TestResolveRoutingOverridesDefault(t *testing.T) {
	c := &client{cfg: config{defaultModel: "anthropic/claude-sonnet-5", defaultThinking: "high"}}

	var created agentSessionEvent
	created.Action = "created"
	created.AgentSession.Comment.Body = "do the thing\npibot: model=ollama/qwen2.5-coder:7b thinking=off"
	if model, thinking := c.resolveRouting(created); model != "ollama/qwen2.5-coder:7b" || thinking != "off" {
		t.Fatalf("resolveRouting(created) = %q, %q", model, thinking)
	}

	var prompted agentSessionEvent
	prompted.Action = "prompted"
	prompted.AgentActivity.Content.Body = "pibot: model=ollama/qwen2.5-coder:7b"
	if model, thinking := c.resolveRouting(prompted); model != "ollama/qwen2.5-coder:7b" || thinking != "high" {
		t.Fatalf("resolveRouting(prompted) = %q, %q, want override model + default thinking", model, thinking)
	}

	var plain agentSessionEvent
	plain.Action = "created"
	plain.AgentSession.Comment.Body = "no directive here"
	if model, thinking := c.resolveRouting(plain); model != "anthropic/claude-sonnet-5" || thinking != "high" {
		t.Fatalf("resolveRouting(plain) = %q, %q, want defaults", model, thinking)
	}
}

func TestModelAllowed(t *testing.T) {
	permissive := &client{cfg: config{}}
	if !permissive.modelAllowed("anything/goes") {
		t.Fatal("empty allowlist should allow any model")
	}

	restricted := &client{cfg: config{allowedModels: []string{"anthropic/claude-sonnet-5", "ollama/qwen2.5-coder:7b"}}}
	if !restricted.modelAllowed("ollama/qwen2.5-coder:7b") {
		t.Fatal("expected known model to be allowed")
	}
	if restricted.modelAllowed("ollama/typo-model") {
		t.Fatal("expected unknown model to be rejected")
	}
}

func TestSplitNonEmpty(t *testing.T) {
	if got := splitNonEmpty("", ","); got != nil {
		t.Fatalf("splitNonEmpty(\"\") = %v, want nil", got)
	}
	got := splitNonEmpty(" a , b ,, c ", ",")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("splitNonEmpty = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitNonEmpty = %v, want %v", got, want)
		}
	}
}
