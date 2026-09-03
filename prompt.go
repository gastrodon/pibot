package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// systemPrompt is the static operating manual, embedded verbatim. It is
// byte-identical on every dispatch — no per-dispatch templating — so it's a
// stable prefix a provider's prompt cache can hit across dispatches, not just
// across turns within one session.
//
//go:embed module/pi-agent-system-prompt.md
var systemPrompt string

// maxDispatchPayload leaves headroom under Nomad's hard-coded 16384-byte
// dispatch payload limit (DispatchPayloadSizeLimit in nomad/job_endpoint.go —
// not a server setting). buildPrompt budgets against this directly instead of
// truncating an already-assembled payload after the fact.
const maxDispatchPayload = 15 * 1024

// Trigger classifies what kind of event opened or continued this session,
// independent of which of Linear's several webhook shapes carried it — see
// resolveTrigger in linear.go. TriggerReassignment exists for forward
// compatibility only: nothing in the data available today can detect it, so
// resolveTrigger never produces it yet.
type Trigger string

const (
	TriggerAssignment   Trigger = "assignment"   // no real triggering comment (plain assignment, or an @-mention embedded in the issue description)
	TriggerMention      Trigger = "mention"      // a real comment triggered this session (top-level or reply — undifferentiated for now)
	TriggerPrompted     Trigger = "prompted"     // a follow-up reply in an already-open session
	TriggerReassignment Trigger = "reassignment" // not yet detectable; reserved
)

// issueRef is the minimal issue identity carried in Frontmatter — never the
// issue's title/description/body. pibot fetches those itself on demand (see
// the system prompt's "Fetching more Linear context" section) rather than
// having them pushed on every dispatch.
type issueRef struct {
	Identifier string
	URL        string
	Team       string
}

// threadMessage is one message in the triggering thread, oldest-first.
type threadMessage struct {
	Author string
	Body   string
}

// sessionContext is everything buildPrompt needs to assemble the user prompt,
// resolved from Linear via session_id alone (see fetchSessionContext) —
// independent of which webhook shape triggered this dispatch.
type sessionContext struct {
	Issue     issueRef
	Summary   string // agentSession.summary; empty in every capture so far, but cheap to carry
	Trigger   Trigger
	Requester string          // empty when the trigger has no real authored comment (TriggerAssignment)
	Request   string          // the triggering message's own text; empty for TriggerAssignment
	Thread    []threadMessage // the rest of the thread, oldest-first, excluding the trigger message itself
}

// Frontmatter is this dispatch's identity, rendered at the top of the user
// prompt. Issue/IssueURL/Team/Trigger are always populated; Requester and
// SessionSummary are additive — rendered only when Linear actually gave us
// something.
type Frontmatter struct {
	Issue          string
	IssueURL       string
	Team           string
	Trigger        Trigger
	Requester      string
	SessionSummary string
}

// render hand-formats Frontmatter as a small YAML-ish block. There's no YAML
// library dependency here on purpose: the field set is flat, string-only, and
// nothing ever parses this back — it only needs to read unambiguously to
// pibot, not round-trip through a real YAML parser.
func (f Frontmatter) render() string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "issue: %s\n", f.Issue)
	fmt.Fprintf(&b, "issue_url: %s\n", f.IssueURL)
	fmt.Fprintf(&b, "team: %s\n", f.Team)
	fmt.Fprintf(&b, "trigger: %s\n", f.Trigger)
	if f.Requester != "" {
		fmt.Fprintf(&b, "requester: %s\n", yamlScalar(f.Requester))
	}
	if f.SessionSummary != "" {
		fmt.Fprintf(&b, "session_summary: %s\n", yamlScalar(f.SessionSummary))
	}
	b.WriteString("---\n")
	return b.String()
}

// yamlScalar renders free text (an author's display name, a session summary —
// never a Linear-controlled identifier) safely enough for a "key: value" block
// read by eye: collapse embedded newlines so a value can never masquerade as
// another frontmatter line, and quote it if it contains a colon so it can't be
// misread as introducing one.
func yamlScalar(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if strings.Contains(s, ": ") || strings.HasPrefix(s, `"`) {
		return strconv.Quote(s)
	}
	return s
}

// frontmatter projects sessionContext down to what Frontmatter.render needs.
func (sc sessionContext) frontmatter() Frontmatter {
	return Frontmatter{
		Issue:          sc.Issue.Identifier,
		IssueURL:       sc.Issue.URL,
		Team:           sc.Issue.Team,
		Trigger:        sc.Trigger,
		Requester:      sc.Requester,
		SessionSummary: sc.Summary,
	}
}

// renderUserPrompt assembles frontmatter + thread + request, in that order —
// general (this dispatch's identity) to specific (the actual ask). A
// TriggerAssignment dispatch has no real request text, so it gets a fixed
// instruction pointing at the system prompt's fetch-it-yourself guidance
// instead of an empty or placeholder "## Request" section.
func renderUserPrompt(sc sessionContext) string {
	var b strings.Builder
	b.WriteString(sc.frontmatter().render())

	if len(sc.Thread) > 0 {
		b.WriteString("\n## Thread\n\n")
		for _, m := range sc.Thread {
			author := m.Author
			if author == "" {
				author = "someone"
			}
			fmt.Fprintf(&b, "### %s\n\n%s\n\n", author, m.Body)
		}
	}

	b.WriteString("\n## Request\n\n")
	if sc.Request != "" {
		b.WriteString(sc.Request)
	} else {
		b.WriteString("You were assigned this issue with no accompanying message. " +
			"Start by fetching the issue's own title and description yourself " +
			"(see \"Fetching more Linear context\" in your operating instructions) " +
			"to see what's being asked.")
	}
	b.WriteString("\n")
	return b.String()
}

// payloadSize returns the exact byte size the {system, prompt} object will
// occupy in the Nomad dispatch Payload — real JSON-escaping overhead
// included, not an estimate — so buildPrompt's budget check matches what
// actually gets sent.
func payloadSize(system, prompt string) (int, error) {
	b, err := json.Marshal(map[string]string{"system": system, "prompt": prompt})
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// buildPrompt assembles the system prompt (always the static, embedded
// manual, untouched) and the user prompt from sc, dropping the oldest thread
// messages first if the result doesn't fit Nomad's dispatch limit. If it
// still doesn't fit with the thread dropped entirely, it fails outright
// rather than truncate the request or send a partial, confusing prompt.
// Deliberate: a failure here is a diagnosable signal (the assembled context
// was too big) rather than an incident hidden behind a workaround, and it's
// cheaper than standing up a channel that bypasses Nomad's dispatch limit
// for what should be a rare case.
func buildPrompt(sc sessionContext) (system, prompt string, err error) {
	prompt = renderUserPrompt(sc)
	size, err := payloadSize(systemPrompt, prompt)
	if err != nil {
		return "", "", fmt.Errorf("marshal prompt payload: %w", err)
	}

	for size > maxDispatchPayload && len(sc.Thread) > 0 {
		sc.Thread = sc.Thread[1:] // drop the oldest message first — recency matters most
		prompt = renderUserPrompt(sc)
		if size, err = payloadSize(systemPrompt, prompt); err != nil {
			return "", "", fmt.Errorf("marshal prompt payload: %w", err)
		}
	}

	if size > maxDispatchPayload {
		return "", "", fmt.Errorf(
			"assembled prompt is %d bytes, over the %d byte dispatch limit even with the thread dropped entirely",
			size, maxDispatchPayload,
		)
	}
	return systemPrompt, prompt, nil
}
