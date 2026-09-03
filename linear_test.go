package main

import "testing"

func TestResolveTriggerCreatedAssignment(t *testing.T) {
	top := sessionTop{
		Comment: &commentRef{ID: "c1", Body: "This thread is for an agent session with pibot.", IsArtificialAgentSessionRoot: true},
	}
	trigger, comment, err := resolveTrigger("created", top)
	if err != nil {
		t.Fatalf("resolveTrigger: %v", err)
	}
	if trigger != TriggerAssignment {
		t.Fatalf("trigger = %q, want %q", trigger, TriggerAssignment)
	}
	if comment.ID != "c1" {
		t.Fatalf("comment = %+v, want the session's own comment", comment)
	}
}

func TestResolveTriggerCreatedMention(t *testing.T) {
	top := sessionTop{
		Comment: &commentRef{ID: "c1", Body: "please do X", IsArtificialAgentSessionRoot: false},
	}
	trigger, _, err := resolveTrigger("created", top)
	if err != nil {
		t.Fatalf("resolveTrigger: %v", err)
	}
	if trigger != TriggerMention {
		t.Fatalf("trigger = %q, want %q", trigger, TriggerMention)
	}
}

func TestResolveTriggerPromptedUsesActivitySourceComment(t *testing.T) {
	top := sessionTop{
		// A stale opening comment must not win over the follow-up.
		Comment: &commentRef{ID: "opening", Body: "@pibot lgtm"},
	}
	top.Activities.Nodes = []struct {
		SourceComment *commentRef `json:"sourceComment"`
	}{
		{SourceComment: &commentRef{ID: "followup", Body: "go ahead and merge this"}},
	}
	trigger, comment, err := resolveTrigger("prompted", top)
	if err != nil {
		t.Fatalf("resolveTrigger: %v", err)
	}
	if trigger != TriggerPrompted {
		t.Fatalf("trigger = %q, want %q", trigger, TriggerPrompted)
	}
	if comment.ID != "followup" || comment.Body != "go ahead and merge this" {
		t.Fatalf("comment = %+v, want the activity's sourceComment, not the stale opening comment", comment)
	}
}

func TestResolveTriggerPromptedWithNoSourceCommentErrors(t *testing.T) {
	if _, _, err := resolveTrigger("prompted", sessionTop{}); err == nil {
		t.Fatal("resolveTrigger(prompted, no activities) = nil error, want an error to trigger the fallback path")
	}
}

func TestParseTopDecodesIssueAndComment(t *testing.T) {
	raw := []byte(`{
  "data": {
    "agentSession": {
      "summary": "pibot cleanup lgtm",
      "issue": {"identifier": "EVA-163", "url": "https://linear.app/x", "team": {"key": "EVA"}},
      "comment": {"id": "c1", "body": "hello", "parentId": "", "isArtificialAgentSessionRoot": false, "user": {"name": "mail@gastrodon.io"}}
    }
  }
}`)
	top, err := parseTop(raw)
	if err != nil {
		t.Fatalf("parseTop: %v", err)
	}
	if top.Issue.Identifier != "EVA-163" || top.Issue.Team.Key != "EVA" {
		t.Fatalf("top.Issue = %+v, want EVA-163/EVA", top.Issue)
	}
	if top.Summary != "pibot cleanup lgtm" {
		t.Fatalf("top.Summary = %q", top.Summary)
	}
	if top.Comment == nil || top.Comment.User.name() != "mail@gastrodon.io" {
		t.Fatalf("top.Comment = %+v", top.Comment)
	}
}

func TestParseThreadOldestFirstExcludingTrigger(t *testing.T) {
	// children arrive newest-first, per Linear's connection ordering.
	raw := []byte(`{
  "data": {
    "comment": {
      "id": "root",
      "body": "root body",
      "user": {"name": "alice"},
      "children": {
        "nodes": [
          {"id": "trigger", "body": "the new reply", "user": {"name": "bob"}},
          {"id": "middle", "body": "middle reply", "user": {"name": "alice"}}
        ]
      }
    }
  }
}`)
	msgs, err := parseThread(raw, "trigger")
	if err != nil {
		t.Fatalf("parseThread: %v", err)
	}
	want := []threadMessage{
		{Author: "alice", Body: "root body"},
		{Author: "alice", Body: "middle reply"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("parseThread() = %+v, want %+v", msgs, want)
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Fatalf("parseThread()[%d] = %+v, want %+v (oldest-first, trigger excluded)", i, msgs[i], want[i])
		}
	}
}

func TestParseThreadRootIsTheTriggerWithNoReplies(t *testing.T) {
	raw := []byte(`{
  "data": {
    "comment": {
      "id": "root",
      "body": "root body",
      "user": {"name": "alice"},
      "children": {"nodes": []}
    }
  }
}`)
	msgs, err := parseThread(raw, "root")
	if err != nil {
		t.Fatalf("parseThread: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("parseThread() = %+v, want empty when the root is itself the excluded trigger with no replies", msgs)
	}
}
