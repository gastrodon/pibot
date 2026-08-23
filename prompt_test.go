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

	got := buildPrompt(ev, threadContext{})
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

	got := buildPrompt(ev, threadContext{})
	if !strings.Contains(got, truncatedMarker) {
		t.Fatal("expected the oversized description to be capped")
	}
	if !strings.Contains(got, strings.Repeat("r", maxFieldBytes*2)) {
		t.Fatal("expected the triggering request body to survive uncapped")
	}
}

func TestBuildPromptEmptyEvent(t *testing.T) {
	if got := buildPrompt(agentSessionEvent{}, threadContext{}); got != "" {
		t.Fatalf("expected an empty prompt for an empty event, got %q", got)
	}
}

func TestBuildPromptIncludesThreadContext(t *testing.T) {
	var ev agentSessionEvent
	ev.Action = "prompted"
	ev.AgentActivity.Content.Body = "what's the status"
	tc := threadContext{
		comments:   []string{"alice: first comment", "bob: second comment"},
		activities: []string{"Response: opened PR #14"},
	}

	got := buildPrompt(ev, tc)
	for _, want := range []string{"## Issue comment thread", "first comment", "second comment", "## Prior agent activity in this session", "opened PR #14", "## Request\nwhat's the status"} {
		if !strings.Contains(got, want) {
			t.Fatalf("buildPrompt output missing %q, got %q", want, got)
		}
	}
}

func TestBuildPromptFiltersAckAndDuplicateTrigger(t *testing.T) {
	var ev agentSessionEvent
	ev.Action = "prompted"
	ev.AgentActivity.Content.Body = "do more"
	tc := threadContext{
		activities: []string{
			"Response: earlier work summary",
			"Thought: " + ackThought,
			"Prompt: do more",
		},
	}

	got := buildPrompt(ev, tc)
	if !strings.Contains(got, "earlier work summary") {
		t.Fatal("expected real prior activity to survive filtering")
	}
	if strings.Contains(got, ackThought) {
		t.Fatalf("expected the boilerplate ack thought to be filtered out, got %q", got)
	}
	if strings.Count(got, "do more") != 1 {
		t.Fatalf("expected the triggering prompt to appear once (in ## Request), got %q", got)
	}
}
