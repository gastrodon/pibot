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

// dispatchNomad kicks the parameterized batch job, passing the (possibly
// shrunk, see shrinkPayload) webhook as the dispatch payload and the session
// id as dispatch meta.
func (c *client) dispatchNomad(ctx context.Context, ev agentSessionEvent, raw []byte, model, thinking string) error {
	// The receiver is the sole owner of the refresh token; hand the job only a
	// short-lived access token (no refresh material) so it can post one response
	// activity without a second refresher rotating tokens out from under us.
	token, err := c.token(ctx)
	if err != nil {
		return fmt.Errorf("get access token for dispatch: %w", err)
	}
	payload := shrinkPayload(raw)
	if len(payload) != len(raw) {
		log.Printf("session %s: webhook payload %d bytes exceeds Nomad's dispatch limit, shrunk to %d", ev.AgentSession.ID, len(raw), len(payload))
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
