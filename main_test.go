package main

import (
	"strings"
	"testing"
)

func TestClipNoop(t *testing.T) {
	if got := clip("short", 100); got != "short" {
		t.Fatalf("clip should pass short strings through unchanged, got %q", got)
	}
}

func TestClipKeepsTail(t *testing.T) {
	big := strings.Repeat("x", 100) + "tail-end"
	got := clip(big, 20)
	if !strings.Contains(got, truncatedMarker) {
		t.Fatalf("expected truncation marker, got %q", got)
	}
	if !strings.HasSuffix(got, "tail-end") {
		t.Fatalf("expected the tail of the original string to survive, got %q", got)
	}
}

func TestBuildPromptIncludesIssueAndRequest(t *testing.T) {
	var ev agentSessionEvent
	ev.Action = "created"
	ev.AgentSession.Issue.Identifier = "EVA-1"
	ev.AgentSession.Issue.Title = "Do the thing"
	ev.AgentSession.Issue.URL = "https://linear.app/x/issue/EVA-1"
	ev.AgentSession.Issue.Description = "issue body"
	ev.AgentSession.Comment.Body = "please do the thing"

	got := buildPrompt(ev)
	for _, want := range []string{"Do the thing", "EVA-1", "https://linear.app/x/issue/EVA-1", "issue body", "please do the thing"} {
		if !strings.Contains(got, want) {
			t.Fatalf("buildPrompt output missing %q, got %q", want, got)
		}
	}
}

func TestBuildPromptCapsLongFieldsButNotTheRequest(t *testing.T) {
	var ev agentSessionEvent
	ev.Action = "prompted"
	ev.AgentSession.Issue.Title = "x"
	ev.AgentSession.Issue.Description = strings.Repeat("d", maxFieldBytes*2)
	ev.AgentActivity.Content.Body = strings.Repeat("r", maxFieldBytes*2)

	got := buildPrompt(ev)
	if !strings.Contains(got, truncatedMarker) {
		t.Fatal("expected the oversized description to be capped")
	}
	if !strings.Contains(got, strings.Repeat("r", maxFieldBytes*2)) {
		t.Fatal("expected the triggering request body to survive uncapped")
	}
}

func TestBuildPromptEmptyEvent(t *testing.T) {
	if got := buildPrompt(agentSessionEvent{}); got != "" {
		t.Fatalf("expected an empty prompt for an empty event, got %q", got)
	}
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
