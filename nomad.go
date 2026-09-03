package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

// fallbackSessionContext builds a degraded sessionContext directly from the
// webhook, for when fetchSessionContext's Linear round trip fails. It carries
// only what the webhook itself already has: no thread, and the trigger
// classified as best it can from the action field alone —
// fetchSessionContext is the only place that can tell a plain assignment
// from a real mention (via isArtificialAgentSessionRoot), so a degraded
// prompt calls it TriggerMention either way rather than guessing further.
func fallbackSessionContext(ev agentSessionEvent) sessionContext {
	trigger := TriggerMention
	if ev.Action == "prompted" {
		trigger = TriggerPrompted
	}
	return sessionContext{
		Issue: issueRef{
			Identifier: ev.AgentSession.Issue.Identifier,
			URL:        ev.AgentSession.Issue.URL,
			Team:       ev.AgentSession.Issue.Team.Key,
		},
		Trigger: trigger,
		Request: ev.triggerBody(),
	}
}

// dispatchNomad resolves this session's context (thread + issue identity,
// fetched fresh from Linear — see fetchSessionContext), builds the system and
// user prompt from it, and kicks the parameterized batch job with both as
// the dispatch payload.
func (c *client) dispatchNomad(ctx context.Context, ev agentSessionEvent, model, thinking string) error {
	// The receiver is the sole owner of the refresh token; hand the job only a
	// short-lived access token (no refresh material) so it can post one response
	// activity without a second refresher rotating tokens out from under us.
	token, err := c.token(ctx)
	if err != nil {
		return fmt.Errorf("get access token for dispatch: %w", err)
	}

	sc, err := c.fetchSessionContext(ctx, ev.AgentSession.ID, ev.Action)
	if err != nil {
		log.Printf("session %s: fetch context failed, falling back to a narrower prompt: %v", ev.AgentSession.ID, err)
		sc = fallbackSessionContext(ev)
	}

	system, prompt, err := buildPrompt(sc)
	if err != nil {
		return fmt.Errorf("build prompt: %w", err)
	}
	payload, err := json.Marshal(map[string]string{"system": system, "prompt": prompt})
	if err != nil {
		return fmt.Errorf("marshal dispatch payload: %w", err)
	}

	body := map[string]any{
		"Payload": base64.StdEncoding.EncodeToString(payload),
		"Meta": map[string]string{
			"session_id":   ev.AgentSession.ID,
			"action":       ev.Action,
			"access_token": token,
			"model":        model,
			"thinking":     thinking,
		},
	}
	buf, _ := json.Marshal(body)
	reqURL := c.cfg.nomadAddr + "/v1/job/" + c.cfg.nomadJob + "/dispatch"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.nomadToken != "" {
		req.Header.Set("X-Nomad-Token", c.cfg.nomadToken)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("nomad %d: %s", resp.StatusCode, out)
	}
	return nil
}
