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

// dispatchNomad kicks the parameterized batch job, passing a curated prompt
// (see buildPrompt) as the dispatch payload and the session id as dispatch
// meta.
func (c *client) dispatchNomad(ctx context.Context, ev agentSessionEvent, model, thinking string) error {
	// The receiver is the sole owner of the refresh token; hand the job only a
	// short-lived access token (no refresh material) so it can post one response
	// activity without a second refresher rotating tokens out from under us.
	token, err := c.token(ctx)
	if err != nil {
		return fmt.Errorf("get access token for dispatch: %w", err)
	}
	// Fetches the issue's comment thread and this session's prior activity; a
	// fetch failure degrades to a narrower prompt rather than blocking dispatch.
	tc, err := c.fetchThreadContext(ctx, token, ev.AgentSession.ID)
	if err != nil {
		log.Printf("session %s: fetch thread context failed, dispatching without it: %v", ev.AgentSession.ID, err)
	}
	payload, err := json.Marshal(map[string]string{"prompt": buildPrompt(ev, tc)})
	if err != nil {
		return fmt.Errorf("marshal dispatch payload: %w", err)
	}
	if len(payload) > maxDispatchPayload {
		// Defensive only: buildPrompt already caps every field it draws from, so
		// this should be unreachable outside pathological input.
		prompt := clip(buildPrompt(ev, tc), maxDispatchPayload-256)
		payload, _ = json.Marshal(map[string]string{"prompt": prompt})
		log.Printf("session %s: dispatch prompt %d bytes exceeded Nomad's %d-byte limit even after field caps, hard-truncated", ev.AgentSession.ID, len(payload), maxDispatchPayload)
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
