package main

import "testing"

func TestTenantForRoutesByEitherKey(t *testing.T) {
	c := newClient(config{})
	one := c.putTenant(tenantState{OrgID: "org-1", AppUserID: "app-1", AccessToken: "a1"})
	two := c.putTenant(tenantState{OrgID: "org-2", AppUserID: "app-2", AccessToken: "a2"})

	if got, err := c.tenantFor("org-2", ""); err != nil || got != two {
		t.Fatalf("tenantFor(org-2) = %v, %v, want the org-2 tenant", got, err)
	}
	// appUserId is the fallback key for payload shapes Linear doesn't put
	// organizationId on.
	if got, err := c.tenantFor("", "app-1"); err != nil || got != one {
		t.Fatalf("tenantFor(appUser app-1) = %v, %v, want the org-1 tenant", got, err)
	}
	// With two installs, an unidentifiable payload must not be guessed at.
	if _, err := c.tenantFor("", ""); err == nil {
		t.Fatal("tenantFor with no keys and two installs = nil error, want a refusal rather than a guess")
	}
	if _, err := c.tenantFor("org-unknown", "app-unknown"); err == nil {
		t.Fatal("tenantFor with unknown keys = nil error, want a refusal")
	}
}

// The webhook signing secret belongs to the Linear *app*, so a workspace whose
// install we refused still gets its events delivered here with a valid
// signature. Routing one of those to the workspace we do hold would hand its
// live access token, and a prompt built from someone else's webhook body, to
// the worker.
func TestTenantForRefusesAWorkspaceItDoesNotHold(t *testing.T) {
	c := newClient(config{})
	only := c.putTenant(tenantState{OrgID: "org-mine", AppUserID: "app-mine", AccessToken: "a1"})

	if got, err := c.tenantFor("org-stranger", "app-stranger"); err == nil {
		t.Fatalf("tenantFor(org-stranger) = %v, want a refusal — never fall through to another workspace's credentials", got)
	}
	if got, err := c.tenantFor("org-stranger", "app-mine"); err == nil {
		t.Fatalf("tenantFor = %v, want the named workspace to be decisive even when a stale app user id matches", got)
	}
	// We know this install's app user id, so a different one is a foreign
	// workspace rather than an unlabelled event — no fallback.
	if got, err := c.tenantFor("", "app-stranger"); err == nil {
		t.Fatalf("tenantFor(appUser app-stranger) = %v, want a refusal — the sole install has a known, different app user", got)
	}
	// The fallback still covers the shape it exists for: a payload that named
	// no workspace at all, against a receiver with exactly one answer.
	if got, err := c.tenantFor("", ""); err != nil || got != only {
		t.Fatalf("tenantFor with no keys and one install = %v, %v, want the sole tenant", got, err)
	}
}

// identify's viewer query is best-effort, so an install can legitimately have
// no app user id on file. Then an appUserId we can't match really is
// unidentified rather than foreign, and the sole install is the only answer.
func TestTenantForFallsBackWhenTheInstallHasNoAppUserID(t *testing.T) {
	c := newClient(config{})
	only := c.putTenant(tenantState{OrgID: "org-mine", AccessToken: "a1"})

	if got, err := c.tenantFor("", "app-unknown"); err != nil || got != only {
		t.Fatalf("tenantFor(appUser app-unknown) = %v, %v, want the sole tenant — nothing on file could have matched", got, err)
	}
}

// The allowlist has to hold on the webhook path too, not just at install:
// install() cannot refuse before the code exchange, because it needs a token
// to ask Linear who consented.
func TestTenantForEnforcesTheAllowlist(t *testing.T) {
	c := newClient(config{allowedOrgs: []string{"org-allowed"}})
	c.putTenant(tenantState{OrgID: "org-allowed", AccessToken: "a1"})
	// A workspace that somehow got into the store, but is no longer listed.
	c.putTenant(tenantState{OrgID: "org-revoked", AccessToken: "a2"})

	if _, err := c.tenantFor("org-allowed", ""); err != nil {
		t.Fatalf("tenantFor(org-allowed) = %v, want the install", err)
	}
	if got, err := c.tenantFor("org-revoked", ""); err == nil {
		t.Fatalf("tenantFor(org-revoked) = %v, want a refusal — the allowlist is re-checked per event", got)
	}

	// The check has to cover the fallback too, not just the named path: a
	// receiver whose one install was dropped from the allowlist serves
	// nothing, however the event identifies itself.
	sole := newClient(config{allowedOrgs: []string{"org-allowed"}})
	sole.putTenant(tenantState{OrgID: "org-revoked", AccessToken: "a1"})
	if got, err := sole.tenantFor("", ""); err == nil {
		t.Fatalf("tenantFor via the sole-install fallback = %v, want a refusal for a workspace off the allowlist", got)
	}
}

func TestTenantForWithNothingInstalled(t *testing.T) {
	c := newClient(config{})
	for _, keys := range [][2]string{{"org-1", ""}, {"", "app-1"}, {"", ""}} {
		if _, err := c.tenantFor(keys[0], keys[1]); err == nil {
			t.Fatalf("tenantFor(%q, %q) with nothing installed = nil error, want a refusal", keys[0], keys[1])
		}
	}
}

func TestPutTenantReindexesARotatedAppUser(t *testing.T) {
	c := newClient(config{})
	c.putTenant(tenantState{OrgID: "org-1", AppUserID: "old", AccessToken: "a1"})
	updated := c.putTenant(tenantState{OrgID: "org-1", AppUserID: "new", AccessToken: "a2"})

	if got, err := c.tenantFor("", "new"); err != nil || got != updated {
		t.Fatalf("tenantFor(appUser new) = %v, %v, want the reindexed tenant", got, err)
	}
	if len(c.byOrg) != 1 {
		t.Fatalf("byOrg = %d entries, want 1 — reinstalling a workspace replaces it", len(c.byOrg))
	}
	// The stale index entry must not survive, or it would keep routing to a
	// workspace identity Linear no longer uses.
	if _, ok := c.byAppUser["old"]; ok {
		t.Fatal("the previous app user id is still indexed")
	}
	if updated.st.AccessToken != "a2" {
		t.Fatalf("tenant token = %q, want the reinstalled one", updated.st.AccessToken)
	}
}

func TestTopLevelFields(t *testing.T) {
	got := topLevelFields([]byte(`{"type":"AgentSessionEvent","organizationId":"o","action":"created"}`))
	if want := "action,organizationId,type"; got != want {
		t.Fatalf("topLevelFields = %q, want %q (sorted, so a log line is comparable across events)", got, want)
	}
	if got := topLevelFields([]byte("not json")); got != "<unparseable>" {
		t.Fatalf("topLevelFields(garbage) = %q", got)
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
