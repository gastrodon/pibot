package main

import (
	"strings"
	"testing"
)

func TestFrontmatterRenderRequiredAlwaysPresentOptionalOmitted(t *testing.T) {
	f := Frontmatter{Issue: "EVA-420", IssueURL: "https://linear.app/x", Team: "EVA", Trigger: TriggerMention}
	out := f.render()
	for _, want := range []string{"issue: EVA-420", "issue_url: https://linear.app/x", "team: EVA", "trigger: mention"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render() = %q, want it to contain %q", out, want)
		}
	}
	for _, absent := range []string{"requester:", "session_summary:"} {
		if strings.Contains(out, absent) {
			t.Fatalf("render() = %q, expected %q to be omitted when unset", out, absent)
		}
	}
}

func TestFrontmatterRenderAdditiveFieldsAppearWhenSet(t *testing.T) {
	f := Frontmatter{
		Issue: "EVA-420", IssueURL: "https://linear.app/x", Team: "EVA", Trigger: TriggerPrompted,
		Requester:      "mail@gastrodon.io",
		SessionSummary: "pibot cleanup lgtm",
	}
	out := f.render()
	if !strings.Contains(out, "requester: mail@gastrodon.io") {
		t.Fatalf("render() = %q, want requester present", out)
	}
	if !strings.Contains(out, "session_summary: pibot cleanup lgtm") {
		t.Fatalf("render() = %q, want session_summary present", out)
	}
}

func TestYamlScalarQuotesColonAndCollapsesNewlines(t *testing.T) {
	if got := yamlScalar("plain name"); got != "plain name" {
		t.Fatalf("yamlScalar(plain) = %q, want unquoted passthrough", got)
	}
	if got := yamlScalar("note: with a colon"); !strings.HasPrefix(got, `"`) {
		t.Fatalf("yamlScalar(with colon) = %q, want it quoted", got)
	}
	if got := yamlScalar("line one\nline two"); strings.Contains(got, "\n") {
		t.Fatalf("yamlScalar(multiline) = %q, want embedded newlines collapsed", got)
	}
}

func TestRenderUserPromptAssignmentHasNoRealRequest(t *testing.T) {
	sc := sessionContext{
		Issue:   issueRef{Identifier: "EVA-420", URL: "https://linear.app/x", Team: "EVA"},
		Trigger: TriggerAssignment,
		// Request and Thread intentionally empty — nothing to attribute to a human.
	}
	out := renderUserPrompt(sc)
	if strings.Contains(out, "## Thread") {
		t.Fatalf("renderUserPrompt(assignment) = %q, want no Thread section with an empty thread", out)
	}
	if !strings.Contains(out, "Fetching more Linear context") {
		t.Fatalf("renderUserPrompt(assignment) = %q, want the fixed fetch-it-yourself instruction", out)
	}
}

func TestRenderUserPromptOrdersFrontmatterThreadRequest(t *testing.T) {
	sc := sessionContext{
		Issue:   issueRef{Identifier: "EVA-420", URL: "https://linear.app/x", Team: "EVA"},
		Trigger: TriggerPrompted,
		Request: "go ahead and merge this",
		Thread:  []threadMessage{{Author: "mail@gastrodon.io", Body: "lgtm"}},
	}
	out := renderUserPrompt(sc)
	fm := strings.Index(out, "issue: EVA-420")
	thread := strings.Index(out, "## Thread")
	request := strings.Index(out, "## Request")
	if fm < 0 || thread < 0 || request < 0 {
		t.Fatalf("renderUserPrompt() = %q, missing an expected section", out)
	}
	if !(fm < thread && thread < request) {
		t.Fatalf("renderUserPrompt() sections out of order: frontmatter=%d thread=%d request=%d", fm, thread, request)
	}
}

func TestBuildPromptClipsOldestThreadMessagesFirst(t *testing.T) {
	sc := sessionContext{
		Issue:   issueRef{Identifier: "EVA-1", URL: "https://linear.app/x", Team: "EVA"},
		Trigger: TriggerPrompted,
		Request: "the actual ask",
	}
	// Enough padding per message, and enough messages, to force clipping under
	// the ~15KB budget once added to the (small) static system prompt.
	pad := strings.Repeat("x", 500)
	for i := 0; i < 40; i++ {
		sc.Thread = append(sc.Thread, threadMessage{Author: "author", Body: pad})
	}
	sc.Thread[0].Body = "OLDEST-MARKER-" + pad
	sc.Thread[len(sc.Thread)-1].Body = "NEWEST-MARKER-" + pad

	system, prompt, err := buildPrompt(sc)
	if err != nil {
		t.Fatalf("buildPrompt() error = %v, want it to fit after clipping", err)
	}
	size, err := payloadSize(system, prompt)
	if err != nil {
		t.Fatalf("payloadSize: %v", err)
	}
	if size > maxDispatchPayload {
		t.Fatalf("buildPrompt() payload = %d bytes, want <= %d", size, maxDispatchPayload)
	}
	if strings.Contains(prompt, "OLDEST-MARKER") {
		t.Fatalf("buildPrompt() kept the oldest message, want it dropped first")
	}
	if !strings.Contains(prompt, "the actual ask") {
		t.Fatalf("buildPrompt() = %q, want the request never clipped", prompt)
	}
}

func TestBuildPromptFailsOutrightWhenRequestAloneIsOverBudget(t *testing.T) {
	sc := sessionContext{
		Issue:   issueRef{Identifier: "EVA-1", URL: "https://linear.app/x", Team: "EVA"},
		Trigger: TriggerMention,
		Request: strings.Repeat("y", maxDispatchPayload+1000),
	}
	if _, _, err := buildPrompt(sc); err == nil {
		t.Fatal("buildPrompt() error = nil, want a hard failure when even an empty thread doesn't fit")
	}
}
