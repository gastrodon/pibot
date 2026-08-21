package main

import "testing"

func TestParseThreadContext(t *testing.T) {
	raw := []byte(`{
  "data": {
    "agentSession": {
      "activities": {
        "nodes": [
          {"content": {"__typename": "AgentActivityThoughtContent", "body": "newest thought"}},
          {"content": {"__typename": "AgentActivityActionContent", "action": "ran a tool"}},
          {"content": {"__typename": "AgentActivityResponseContent", "body": "oldest response"}}
        ]
      },
      "issue": {
        "comments": {
          "nodes": [
            {"body": "newest comment", "user": {"name": "alice"}},
            {"body": "oldest comment", "user": null}
          ]
        }
      }
    }
  }
}`)

	tc, err := parseThreadContext(raw)
	if err != nil {
		t.Fatalf("parseThreadContext: %v", err)
	}
	wantActivities := []string{"Response: oldest response", "Action: ran a tool", "Thought: newest thought"}
	if len(tc.activities) != len(wantActivities) {
		t.Fatalf("activities = %v, want %v", tc.activities, wantActivities)
	}
	for i, want := range wantActivities {
		if tc.activities[i] != want {
			t.Fatalf("activities[%d] = %q, want %q (order should be oldest-first)", i, tc.activities[i], want)
		}
	}
	wantComments := []string{"someone: oldest comment", "alice: newest comment"}
	if len(tc.comments) != len(wantComments) {
		t.Fatalf("comments = %v, want %v", tc.comments, wantComments)
	}
	for i, want := range wantComments {
		if tc.comments[i] != want {
			t.Fatalf("comments[%d] = %q, want %q (order should be oldest-first)", i, tc.comments[i], want)
		}
	}
}
