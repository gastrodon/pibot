# Linear `AgentSessionEvent` webhook shapes

Ground truth on what Linear's webhook actually sends per trigger path, for
writing the context-packet builder against. This is a living document —
see "Gaps" below for the paths still unverified and how to close them.

## Methodology and its limits

Every entry below except "prompted" comes from a raw payload captured at
`/local/webhook.json` inside a live `pi-agent` Nomad dispatch — the exact
bytes the receiver handed to the worker, sanitized only for ids/tokens.
"Prompted" is carried over from prior analysis (cited, not independently
verified against a raw payload by this pass — see Gaps).

A capture only exists for whichever trigger path actually fired *that*
dispatch, and pibot cannot manufacture the others on demand: pibot's own
comments, assignments, and other authored events do not start new agent
sessions (only a human/other-user action does), so pibot can't trigger
itself into a second shape mid-session. Closing the remaining gaps needs a
human to perform the trigger listed in each gap below; the next dispatch
that fires can capture and append its own `/local/webhook.json`.

## Trigger paths and status

| # | Trigger | Status | Source |
| - | -- | -- | -- |
| 1 | Assignment, no comment | **unverified** | none — needs a human to assign a fresh issue to pibot with no comment |
| 2 | `@`-mention in a top-level comment | inherited, unverified raw | [EVA-189](https://linear.app/gastrodon/issue/EVA-189) prose (Shape A, example EVA-186) |
| 3 | `@`-mention in a reply inside an existing thread | **unverified** | none — needs a human to reply-mention inside a thread that already has other comments |
| 4 | `@`-mention in the issue description | **verified** | this capture (EVA-229) |
| 5 | Follow-up reply in an open session (`prompted`) | inherited, unverified raw | [EVA-189](https://linear.app/gastrodon/issue/EVA-189) prose (Shape B, example EVA-163) |
| 6 | Re-assignment / delegate | **unverified** | none — needs a human to re-assign an issue already assigned to pibot, or use Linear's delegate action |

Both "inherited" rows are described in prose in EVA-189 (top-level key list
and byte sizes) but no raw JSON from those captures is available in the
repo or in Linear to re-verify against — they're included here for
completeness and cross-referenced, not re-confirmed.

## Verified: `created` via `@`-mention in the issue description (#4)

Captured from EVA-229 itself, which has no assignee and no comments other
than the auto-created session-start comment — the dispatch was caused
solely by an `<user notify>pibot</user>` mention tag embedded in the issue
**description** body.

Top-level keys, JSON-encoded byte length of each value:

```
action 9              agentSession 3206      appUserId 38
createdAt 26           guidance 4 (null)      oauthClientId 34
organizationId 38      previousComments 4 (null)
promptContext 2286 (already shrinkPayload-truncated — see below)
type 19                webhookId 38            webhookTimestamp 13
```

Presence:

```
promptContext      present, non-null
guidance           present, null
previousComments   present, null
agentActivity      absent (key not present at all)
agentSession.summary  present, null
```

Full sanitized payload:

```json
{
  "action": "created",
  "agentSession": {
    "id": "<agent-session-id>",
    "createdAt": "2026-08-21T02:05:47.315Z",
    "updatedAt": "2026-08-21T02:05:47.315Z",
    "archivedAt": null,
    "creatorId": "<user-id>",
    "appUserId": "<pibot-app-user-id>",
    "commentId": "<comment-id>",
    "sourceCommentId": null,
    "issueId": "<issue-id>",
    "pullRequestId": null,
    "slugId": "bc79cc25d6182",
    "status": "pending",
    "startedAt": null,
    "endedAt": null,
    "dismissedAt": null,
    "dismissedById": null,
    "externalLink": null,
    "summary": null,
    "sourceMetadata": null,
    "plan": null,
    "workspaceDiff": null,
    "context": [],
    "url": "https://linear.app/gastrodon/issue/EVA-229/discovery-exact-webhook-shape-for-each-pibot-trigger-path#agent-session-420f8700",
    "externalUrls": [],
    "organizationId": "<org-id>",
    "creator": {
      "id": "<user-id>",
      "name": "mail@gastrodon.io",
      "email": "<redacted>",
      "url": "<redacted>"
    },
    "comment": {
      "id": "<comment-id>",
      "body": "This thread is for an agent session with pibot.",
      "issueId": "<issue-id>"
    },
    "issue": {
      "id": "<issue-id>",
      "title": "Discovery: exact webhook shape for each pibot trigger path",
      "teamId": "<team-id>",
      "team": { "id": "<team-id>", "key": "EVA", "name": "Eva" },
      "identifier": "EVA-229",
      "url": "https://linear.app/gastrodon/issue/EVA-229/discovery-exact-webhook-shape-for-each-pibot-trigger-path",
      "description": "<full description body — contains an inline <user id=\"...\" notify>pibot</user> mention tag; see below>"
    }
  },
  "appUserId": "<pibot-app-user-id>",
  "createdAt": "2026-08-21T02:05:47.874Z",
  "guidance": null,
  "oauthClientId": "<oauth-client-id>",
  "organizationId": "<org-id>",
  "previousComments": null,
  "promptContext": "<promptContext, see notes>",
  "type": "AgentSessionEvent",
  "webhookId": "<webhook-id>",
  "webhookTimestamp": 1787277948078
}
```

### Where the mention lives

The `@`-mention is not a separate field — Linear inlines it into
`agentSession.issue.description` as an HTML-ish tag:

```
...<user id="e544b32c-57cc-446b-b18c-bcec44922b70" notify>pibot</user> please do discovery: ...
```

`agentSession.comment.body` is **not** the mention text. On a description
mention (and, per its literal wording, presumably on a plain assignment
too — both start a session without a human typing a comment) Linear
synthesizes a placeholder session-start comment instead:

```
"This thread is for an agent session with pibot."
```

This matters for `webhook.go`'s `triggerBody()`, which returns
`agentSession.comment.body` for the `created` action: on this trigger path
that call returns the placeholder, not the actual request. The real
instruction only exists in `promptContext` / `agentSession.issue.description`
on this shape. `parseDirective`, which reads `triggerBody()`, would
therefore never see a `pibot: model=...` directive placed in an issue
description or on a plain assignment — only one placed in an actual
top-level comment or a `prompted` follow-up. Worth confirming against a
real assignment-only capture (gap #1) before relying on this.

### `promptContext` truncation, live

This capture's `promptContext` was already past `shrinkPayload`'s 15 KiB
budget by the time it reached the worker — the value starts mid-word with
the `…[truncated by pibot: …]…` marker, head end cut, tail (the most
recent thread material) intact. This confirms the tail-keeps/head-cuts
behavior described in `payload.go` and in EVA-189 against a real payload
rather than only the unit tests.

### A structural note not obviously predictable from the schema

`previousComments` is `null` here, where EVA-189's Shape A capture (a
top-level comment mention, on an issue that already had prior comments)
reported ~516 bytes of content in that field. The likely explanation is
that `previousComments` tracks whether the *issue* already had comment
history at trigger time, not which trigger path fired — EVA-229 had none
before this session opened. Worth confirming once gap #2's raw payload (or
a fresh capture of it) is available to compare on equal footing.

## Inherited (unverified raw): `created` via comment mention/assignment (#2)

From EVA-189, citing a capture against EVA-186 (a top-level comment
mention/reply). Top-level key sizes:

```
type 17  action 7  createdAt 24  organizationId 36  oauthClientId 32  appUserId 36
agentSession 2332  previousComments 516  guidance 4 (null)  promptContext 3037
webhookTimestamp 13  webhookId 36
```

`promptContext` is a curated XML document Linear assembles itself:
issue + sub-issues (with descriptions), the triggering comment thread
(`primary-directive-thread`), and other threads on the issue
(`other-thread`), e.g.:

```xml
<issue identifier="EVA-186">
  <title>…</title> <description>…</description> <team name="Eva"/>
  <sub-issues><sub-issue identifier="EVA-187">…</sub-issue></sub-issues>
</issue>
<primary-directive-thread comment-id="...">
  <comment author="mail@gastrodon.io" created-at="...">
    <user id="..." notify>pibot</user> also for this discovery …
  </comment>
</primary-directive-thread>
<other-thread comment-id="...">
  <comment author="mail@gastrodon.io" ...>Assigning to pibot for discovery…</comment>
</other-thread>
```

Unlike the description-mention capture above, `agentSession.comment.body`
here is reported as the actual triggering text, not a placeholder — since
the trigger *was* a real comment. Not re-verified against raw JSON by this
pass; flagged in Gaps for confirmation, and to settle whether "assignment"
and "top-level comment mention" are actually the same shape or were
conflated in the original analysis (see gap #1 below).

## Inherited (unverified raw): `prompted` (#5)

From EVA-189, citing a capture against EVA-163 (a follow-up reply inside
an already-open session). Top-level key sizes:

```
type 17  action 8  createdAt 24  organizationId 36  oauthClientId 32  appUserId 36
agentSession 1755  agentActivity 646  webhookTimestamp 13  webhookId 36
```

No `promptContext`, no `guidance`, no `previousComments` key — reported as
absent from the payload (not present-and-null), though the original
analysis doesn't show the raw JSON to confirm the absent/null distinction
either way; treat that as unconfirmed until a raw capture exists.

The new message lives at `agentActivity.content.body` — **not**
`agentSession.comment.body`, which is the comment that *originally opened*
the session, now stale. `webhook.go`'s `triggerBody()` already branches on
`Action` to read the right field for this shape.

## Gaps and how to close them

Each of these needs a human (not pibot) to perform the trigger once, on
an issue with pibot as a participant, then pibot (or anyone) appends the
new `/local/webhook.json` from that dispatch to this file:

1. **Assignment, no comment.** Assign a fresh issue to pibot without
   commenting or `@`-mentioning it anywhere on the issue. Settles whether
   this is the same shape as description-mention above (both start a
   session with no real triggering comment) or has its own quirks, and
   what `agentSession.comment.body` reads on a pure assignment.
2. **Top-level comment mention, raw payload.** `@`-mention pibot in a new
   top-level comment on an issue with existing comment history, and save
   the resulting `/local/webhook.json` verbatim — to confirm the EVA-189
   prose above against real JSON, particularly the `previousComments`
   presence/size question.
3. **Reply-inside-thread mention.** `@`-mention pibot in a reply nested
   under an existing comment thread (not a fresh top-level comment). The
   open question this settles: does `promptContext`'s
   `primary-directive-thread` scope to just that reply's thread, or does
   Linear widen it? This is the shape the EVA-189 design decision ("if the
   mention is a reply inside a thread: that thread's messages, and only
   that thread's") most needs verified.
4. **`prompted`, raw payload.** Reply inside an already-open pibot agent
   session and save the resulting `/local/webhook.json` — confirms the
   `agentActivity`/absent-fields table above against real JSON.
5. **Re-assignment / delegate.** Re-assign an issue already assigned to
   pibot to someone else and back, or use Linear's delegate action if the
   workspace has one, and capture what (if anything) fires.
