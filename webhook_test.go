package main

import "testing"

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
