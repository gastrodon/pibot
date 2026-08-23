package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	linearGraphQL = "https://api.linear.app/graphql"
	linearOAuth   = "https://api.linear.app/oauth/token"
)

// postActivity emits an agent activity (thought | action | response | error),
// refreshing the OAuth token once on an auth failure and retrying.
func (c *client) postActivity(ctx context.Context, sessionID, typ, body string) error {
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	status, out, err := c.doActivity(ctx, token, sessionID, typ, body)
	if err != nil {
		return err
	}
	if isAuthFailure(status, out) {
		token, err = c.refreshAfter(ctx, token)
		if err != nil {
			return fmt.Errorf("auth failed and refresh failed: %w", err)
		}
		status, out, err = c.doActivity(ctx, token, sessionID, typ, body)
		if err != nil {
			return err
		}
	}
	if status != http.StatusOK || bytes.Contains(out, []byte(`"errors"`)) {
		return fmt.Errorf("graphql %d: %s", status, out)
	}
	return nil
}

// doActivity performs one agentActivityCreate with the given bearer token and
// returns the raw status + body so the caller can decide whether to refresh.
func (c *client) doActivity(ctx context.Context, token, sessionID, typ, body string) (int, []byte, error) {
	const q = `mutation($input: AgentActivityCreateInput!) {
  agentActivityCreate(input: $input) { success }
}`
	payload := map[string]any{
		"query": q,
		"variables": map[string]any{
			"input": map[string]any{
				"agentSessionId": sessionID,
				"content":        map[string]any{"type": typ, "body": body},
			},
		},
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, linearGraphQL, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, nil
}

// threadContext is the issue's comment thread and this session's prior
// activity, fetched directly from Linear rather than carried on the webhook.
// Both are oldest-first; buildPrompt's clip keeps the tail when a section
// runs over budget.
type threadContext struct {
	comments   []string
	activities []string
}

// threadContextQuery fetches the session's activity and, through its issue,
// its comments. Each activity content variant is a distinct GraphQL type, so
// each needs its own inline fragment.
const threadContextQuery = `query($id: String!) {
  agentSession(id: $id) {
    activities(first: 100) {
      nodes {
        content {
          __typename
          ... on AgentActivityThoughtContent { body }
          ... on AgentActivityActionContent { action }
          ... on AgentActivityResponseContent { body }
          ... on AgentActivityPromptContent { body }
          ... on AgentActivityErrorContent { body }
          ... on AgentActivityElicitationContent { body }
        }
      }
    }
    issue {
      comments(first: 100) {
        nodes { body user { name } }
      }
    }
  }
}`

// fetchThreadContext queries Linear for sessionID's comment thread and prior
// session activity, using the same OAuth token postActivity posts with. A
// fetch failure is not fatal to dispatch — see dispatchNomad.
func (c *client) fetchThreadContext(ctx context.Context, token, sessionID string) (threadContext, error) {
	payload := map[string]any{
		"query":     threadContextQuery,
		"variables": map[string]any{"id": sessionID},
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, linearGraphQL, bytes.NewReader(buf))
	if err != nil {
		return threadContext{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return threadContext{}, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return threadContext{}, err
	}
	if resp.StatusCode != http.StatusOK || bytes.Contains(out, []byte(`"errors"`)) {
		return threadContext{}, fmt.Errorf("graphql %d: %s", resp.StatusCode, out)
	}
	return parseThreadContext(out)
}

// parseThreadContext decodes fetchThreadContext's response body into a
// threadContext, split out so decoding is testable without a live API call.
func parseThreadContext(raw []byte) (threadContext, error) {
	var parsed struct {
		Data struct {
			AgentSession struct {
				Activities struct {
					Nodes []struct {
						Content struct {
							Typename string `json:"__typename"`
							Body     string `json:"body"`
							Action   string `json:"action"`
						} `json:"content"`
					} `json:"nodes"`
				} `json:"activities"`
				Issue struct {
					Comments struct {
						Nodes []struct {
							Body string `json:"body"`
							User *struct {
								Name string `json:"name"`
							} `json:"user"`
						} `json:"nodes"`
					} `json:"comments"`
				} `json:"issue"`
			} `json:"agentSession"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return threadContext{}, fmt.Errorf("decode thread context: %w", err)
	}

	var tc threadContext
	// Linear returns both connections newest-first; walk backwards so every
	// slice buildPrompt sees is oldest-first, like every other field it draws
	// from.
	nodes := parsed.Data.AgentSession.Activities.Nodes
	for i := len(nodes) - 1; i >= 0; i-- {
		content := nodes[i].Content
		label := strings.TrimSuffix(strings.TrimPrefix(content.Typename, "AgentActivity"), "Content")
		switch {
		case content.Body != "":
			tc.activities = append(tc.activities, label+": "+content.Body)
		case content.Action != "":
			tc.activities = append(tc.activities, label+": "+content.Action)
		}
	}
	comments := parsed.Data.AgentSession.Issue.Comments.Nodes
	for i := len(comments) - 1; i >= 0; i-- {
		who := "someone"
		if u := comments[i].User; u != nil && u.Name != "" {
			who = u.Name
		}
		tc.comments = append(tc.comments, who+": "+comments[i].Body)
	}
	return tc, nil
}

// isAuthFailure reports whether a Linear response indicates an expired/invalid
// token (worth a refresh + retry). GraphQL auth errors can arrive 200/400 with
// an AUTHENTICATION code rather than a 401.
func isAuthFailure(status int, body []byte) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	return bytes.Contains(bytes.ToLower(body), []byte("authentication"))
}

// token returns a currently-valid access token, refreshing proactively if it's
// within 60s of expiry.
func (c *client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Refresh when we have no access token at all (bootstrap from the refresh
	// token alone) or when a known expiry is within 60s.
	needRefresh := c.tok.AccessToken == "" ||
		(c.tok.Expires > 0 && time.Now().Unix() >= c.tok.Expires-60)
	if needRefresh {
		if err := c.refreshLocked(ctx); err != nil {
			return "", err
		}
	}
	if c.tok.AccessToken == "" {
		return "", fmt.Errorf("no access token available")
	}
	return c.tok.AccessToken, nil
}

// refreshAfter refreshes only if the token still matches `used` (i.e. a
// concurrent caller didn't already refresh), then returns the current token.
func (c *client) refreshAfter(ctx context.Context, used string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok.AccessToken == used {
		if err := c.refreshLocked(ctx); err != nil {
			return "", err
		}
	}
	return c.tok.AccessToken, nil
}

// refreshLocked exchanges the refresh token for a new access token and persists
// the result. Caller must hold c.mu.
func (c *client) refreshLocked(ctx context.Context) error {
	if c.cfg.clientID == "" || c.cfg.clientSecret == "" || c.tok.RefreshToken == "" {
		return fmt.Errorf("cannot refresh: missing client creds or refresh token")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.tok.RefreshToken},
		"client_id":     {c.cfg.clientID},
		"client_secret": {c.cfg.clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, linearOAuth, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token refresh %d: %s", resp.StatusCode, out)
	}
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(out, &tr); err != nil {
		return fmt.Errorf("token refresh decode: %w", err)
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("token refresh: empty access_token in response")
	}
	c.tok.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" { // Linear may rotate the refresh token
		c.tok.RefreshToken = tr.RefreshToken
	}
	if tr.ExpiresIn > 0 {
		c.tok.Expires = time.Now().Unix() + tr.ExpiresIn
	}
	c.persist(c.tok)
	log.Printf("refreshed Linear access token (expires %d)", c.tok.Expires)
	return nil
}
