package main

import (
	"strings"
	"unicode/utf8"
)

// maxDispatchPayload leaves headroom under Nomad's hard-coded 16384-byte
// dispatch payload limit (DispatchPayloadSizeLimit in nomad/job_endpoint.go —
// not a server setting, so we can't raise it).
const maxDispatchPayload = 15 * 1024

// maxFieldBytes bounds any single context component buildPrompt draws from
// (issue description, session summary, comment thread, prior activity) so no
// one field can crowd out Nomad's dispatch limit. The triggering message
// itself is never capped — see buildPrompt.
const maxFieldBytes = 4000

// ackThought is the boilerplate acknowledgement dispatch posts before Nomad
// runs; buildPrompt filters it back out of the fetched activity history so it
// doesn't sit redundantly above ## Request.
const ackThought = "Picking this up — spinning up an agent."

// truncatedMarker prefixes any field clip had to cut down, so pi (and anyone
// reading pi.jsonl) can tell the context it saw was partial.
const truncatedMarker = "…[truncated by pibot: content exceeded the dispatch size budget]…"

// clip truncates s to at most n bytes, keeping the tail — Linear assembles
// context oldest-first, so the newest/most relevant text survives a cut. A
// no-op if s already fits.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return truncatedMarker + trimToUTF8Boundary(s, len(s)-n)
}

// buildPrompt assembles the prompt pi runs on: the issue title/URL/
// description, the session summary, the issue comment thread and prior
// session activity fetchThreadContext pulled from Linear, and finally the
// message that triggered this dispatch.
func buildPrompt(ev agentSessionEvent, tc threadContext) string {
	var b strings.Builder
	if iss := ev.AgentSession.Issue; iss.Title != "" {
		b.WriteString("# " + iss.Title)
		if iss.Identifier != "" {
			b.WriteString(" (" + iss.Identifier + ")")
		}
		b.WriteString("\n\n")
		if iss.URL != "" {
			b.WriteString(iss.URL + "\n\n")
		}
		if iss.Description != "" {
			b.WriteString(clip(iss.Description, maxFieldBytes) + "\n\n")
		}
	}
	if s := ev.AgentSession.Summary; s != "" {
		b.WriteString("## Session summary\n" + clip(s, maxFieldBytes) + "\n\n")
	}
	if len(tc.comments) > 0 {
		b.WriteString("## Issue comment thread\n" + clip(strings.Join(tc.comments, "\n\n"), maxFieldBytes) + "\n\n")
	}
	trigger := ev.triggerBody()
	if activities := filterNoise(tc.activities, trigger); len(activities) > 0 {
		b.WriteString("## Prior agent activity in this session\n" + clip(strings.Join(activities, "\n\n"), maxFieldBytes) + "\n\n")
	}
	if trigger != "" {
		b.WriteString("## Request\n" + trigger)
	}
	return b.String()
}

// filterNoise drops activity lines that would duplicate buildPrompt's other
// sections: dispatch's own ack thought, and (on a re-dispatch) the message
// already rendered verbatim as ## Request.
func filterNoise(activities []string, trigger string) []string {
	skip := map[string]bool{"Thought: " + ackThought: true}
	if trigger != "" {
		skip["Prompt: "+trigger] = true
	}
	out := make([]string, 0, len(activities))
	for _, a := range activities {
		if !skip[a] {
			out = append(out, a)
		}
	}
	return out
}

// trimToUTF8Boundary drops the first n bytes of s, nudged forward to the next
// rune boundary so we never split a multi-byte UTF-8 character.
func trimToUTF8Boundary(s string, n int) string {
	if n <= 0 {
		return s
	}
	if n >= len(s) {
		return ""
	}
	for n < len(s) && !utf8.RuneStart(s[n]) {
		n++
	}
	return s[n:]
}
