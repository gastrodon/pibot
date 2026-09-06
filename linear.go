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

// postActivity emits an agent activity (thought | action | response | error)
// into one of this workspace's sessions, refreshing the OAuth token once on an
// auth failure and retrying.
func (t *tenant) postActivity(ctx context.Context, sessionID, typ, body string) error {
	c := t.c
	token, err := t.token(ctx)
	if err != nil {
		return err
	}
	status, out, err := c.doActivity(ctx, token, sessionID, typ, body)
	if err != nil {
		return err
	}
	if isAuthFailure(status, out) {
		token, err = t.refreshAfter(ctx, token)
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

// userRef is the common {name} shape Linear returns for a comment's author.
type userRef struct {
	Name string `json:"name"`
}

func (u *userRef) name() string {
	if u == nil {
		return ""
	}
	return u.Name
}

// commentRef is one comment as fetched for trigger resolution — enough to
// classify the trigger (isArtificialAgentSessionRoot distinguishes a real
// authored comment from Linear's synthesized session-start placeholder) and,
// if it's a reply, find its thread root (parentId).
type commentRef struct {
	ID                           string   `json:"id"`
	Body                         string   `json:"body"`
	ParentID                     string   `json:"parentId"`
	IsArtificialAgentSessionRoot bool     `json:"isArtificialAgentSessionRoot"`
	User                         *userRef `json:"user"`
}

// sessionTop is agentSession's fields needed to resolve the trigger comment
// and the issue identity, decoded from sessionContextQuery.
type sessionTop struct {
	Summary string `json:"summary"`
	Issue   struct {
		Identifier string `json:"identifier"`
		URL        string `json:"url"`
		Team       struct {
			Key string `json:"key"`
		} `json:"team"`
	} `json:"issue"`
	Comment    *commentRef `json:"comment"`
	Activities struct {
		Nodes []struct {
			SourceComment *commentRef `json:"sourceComment"`
		} `json:"nodes"`
	} `json:"activities"`
}

// sessionContextQuery resolves the trigger comment plus issue identity and
// session summary in one round trip, anchored on session_id alone —
// independent of which webhook shape triggered dispatch. "created" always
// carries the session-opening comment at .comment (real text on a mention, a
// synthesized placeholder on a plain assignment or description-mention);
// "prompted" has no new .comment at all — the follow-up lives at the latest
// activity's sourceComment, which is why activities is fetched here too. See
// resolveTrigger.
const sessionContextQuery = `query($id: String!) {
  agentSession(id: $id) {
    summary
    issue {
      identifier
      url
      team { key }
    }
    comment {
      id
      body
      parentId
      isArtificialAgentSessionRoot
      user { name }
    }
    activities(first: 1) {
      nodes {
        sourceComment {
          id
          body
          parentId
          isArtificialAgentSessionRoot
          user { name }
        }
      }
    }
  }
}`

// threadQuery fetches a thread root and its replies, given the root's
// comment id. Linear returns children newest-first (like every other
// connection here); parseThread reverses it to oldest-first.
const threadQuery = `query($id: String!) {
  comment(id: $id) {
    id
    body
    user { name }
    children(first: 100) {
      nodes {
        id
        body
        user { name }
      }
    }
  }
}`

// resolveTrigger picks the triggering comment out of top per the webhook's
// action, and classifies the trigger. Returns an error if the shape doesn't
// carry what that action is supposed to guarantee (e.g. a "prompted" event
// with no activity sourceComment) — a caller should treat that as a fetch
// failure and degrade to a narrower prompt, not guess.
func resolveTrigger(action string, top sessionTop) (Trigger, commentRef, error) {
	if action == "prompted" {
		if len(top.Activities.Nodes) == 0 || top.Activities.Nodes[0].SourceComment == nil {
			return "", commentRef{}, fmt.Errorf("prompted event with no activity sourceComment")
		}
		return TriggerPrompted, *top.Activities.Nodes[0].SourceComment, nil
	}
	if top.Comment == nil {
		return "", commentRef{}, fmt.Errorf("%s event with no session comment", action)
	}
	if top.Comment.IsArtificialAgentSessionRoot {
		return TriggerAssignment, *top.Comment, nil
	}
	return TriggerMention, *top.Comment, nil
}

// fetchSessionContext resolves sessionContext for sessionID given the
// webhook's action ("created" or "prompted"). Two GraphQL round trips: one to
// find the trigger comment and issue identity, one to fetch its thread. A
// fetch failure here should not block dispatch — callers should fall back to
// a narrower prompt (frontmatter + request, no thread) rather than propagate
// the error to the Linear thread.
func (t *tenant) fetchSessionContext(ctx context.Context, sessionID, action string) (sessionContext, error) {
	c := t.c
	token, err := t.token(ctx)
	if err != nil {
		return sessionContext{}, fmt.Errorf("get access token: %w", err)
	}

	top, err := c.fetchTop(ctx, token, sessionID)
	if err != nil {
		return sessionContext{}, fmt.Errorf("fetch session: %w", err)
	}

	trigger, triggerComment, err := resolveTrigger(action, top)
	if err != nil {
		return sessionContext{}, err
	}

	var requester, request string
	if !triggerComment.IsArtificialAgentSessionRoot {
		requester = triggerComment.User.name()
		request = triggerComment.Body
	}

	rootID := triggerComment.ParentID
	if rootID == "" {
		rootID = triggerComment.ID
	}
	thread, err := c.fetchThread(ctx, token, rootID, triggerComment.ID)
	if err != nil {
		return sessionContext{}, fmt.Errorf("fetch thread: %w", err)
	}

	return sessionContext{
		Issue: issueRef{
			Identifier: top.Issue.Identifier,
			URL:        top.Issue.URL,
			Team:       top.Issue.Team.Key,
		},
		Summary:   top.Summary,
		Trigger:   trigger,
		Requester: requester,
		Request:   request,
		Thread:    thread,
	}, nil
}

// doGraphQL posts one GraphQL query with the given bearer token and returns
// the raw response body. No auth-refresh-retry here (unlike doActivity) — the
// caller already holds a freshly-checked token from c.token, and a
// dispatch-time fetch failure degrades to a narrower prompt rather than
// justifying a second round trip.
func (c *client) doGraphQL(ctx context.Context, token, query string, variables map[string]any) ([]byte, error) {
	payload := map[string]any{"query": query}
	if variables != nil {
		payload["variables"] = variables
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, linearGraphQL, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || bytes.Contains(out, []byte(`"errors"`)) {
		return nil, fmt.Errorf("graphql %d: %s", resp.StatusCode, out)
	}
	return out, nil
}

func (c *client) fetchTop(ctx context.Context, token, sessionID string) (sessionTop, error) {
	out, err := c.doGraphQL(ctx, token, sessionContextQuery, map[string]any{"id": sessionID})
	if err != nil {
		return sessionTop{}, err
	}
	return parseTop(out)
}

// parseTop decodes fetchTop's response body, split out so decoding is
// testable without a live API call.
func parseTop(raw []byte) (sessionTop, error) {
	var parsed struct {
		Data struct {
			AgentSession sessionTop `json:"agentSession"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return sessionTop{}, fmt.Errorf("decode session: %w", err)
	}
	return parsed.Data.AgentSession, nil
}

func (c *client) fetchThread(ctx context.Context, token, rootID, excludeID string) ([]threadMessage, error) {
	out, err := c.doGraphQL(ctx, token, threadQuery, map[string]any{"id": rootID})
	if err != nil {
		return nil, err
	}
	return parseThread(out, excludeID)
}

// parseThread decodes threadQuery's response into oldest-first messages,
// excluding excludeID — the trigger message itself, which buildPrompt renders
// separately as the request rather than as part of the thread. Split out so
// decoding is testable without a live API call.
func parseThread(raw []byte, excludeID string) ([]threadMessage, error) {
	var parsed struct {
		Data struct {
			Comment *struct {
				ID       string   `json:"id"`
				Body     string   `json:"body"`
				User     *userRef `json:"user"`
				Children struct {
					Nodes []struct {
						ID   string   `json:"id"`
						Body string   `json:"body"`
						User *userRef `json:"user"`
					} `json:"nodes"`
				} `json:"children"`
			} `json:"comment"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode thread: %w", err)
	}
	root := parsed.Data.Comment
	if root == nil {
		return nil, fmt.Errorf("thread root comment not found")
	}

	var msgs []threadMessage
	if root.ID != excludeID {
		msgs = append(msgs, threadMessage{Author: root.User.name(), Body: root.Body})
	}
	// children arrive newest-first; walk backwards for oldest-first, matching
	// every other connection this package reads.
	children := root.Children.Nodes
	for i := len(children) - 1; i >= 0; i-- {
		ch := children[i]
		if ch.ID == excludeID {
			continue
		}
		msgs = append(msgs, threadMessage{Author: ch.User.name(), Body: ch.Body})
	}
	return msgs, nil
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

// token returns a currently-valid access token for this workspace, refreshing
// proactively if it's within 60s of expiry.
func (t *tenant) token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Refresh when we hold no access token at all (an install whose access
	// token was already spent) or when a known expiry is within 60s.
	needRefresh := t.st.AccessToken == "" ||
		(t.st.Expires > 0 && time.Now().Unix() >= t.st.Expires-60)
	if needRefresh {
		if err := t.refreshLocked(ctx); err != nil {
			return "", err
		}
	}
	if t.st.AccessToken == "" {
		return "", fmt.Errorf("no access token available for workspace %s", t.st.OrgID)
	}
	return t.st.AccessToken, nil
}

// refreshAfter refreshes only if the token still matches `used` (i.e. a
// concurrent caller didn't already refresh), then returns the current token.
func (t *tenant) refreshAfter(ctx context.Context, used string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.st.AccessToken == used {
		if err := t.refreshLocked(ctx); err != nil {
			return "", err
		}
	}
	return t.st.AccessToken, nil
}

// refreshLocked exchanges this workspace's refresh token for a new access token
// and persists the result — Linear may rotate the refresh token on each use, so
// the rotation has to survive a restart or the install is spent. Caller must
// hold t.mu.
func (t *tenant) refreshLocked(ctx context.Context) error {
	cfg := t.c.cfg
	if cfg.clientID == "" || cfg.clientSecret == "" || t.st.RefreshToken == "" {
		return fmt.Errorf("cannot refresh workspace %s: missing client creds or refresh token", t.st.OrgID)
	}
	tok, err := t.c.postOAuth(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {t.st.RefreshToken},
		"client_id":     {cfg.clientID},
		"client_secret": {cfg.clientSecret},
	})
	if err != nil {
		return fmt.Errorf("token refresh: %w", err)
	}

	t.st.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" { // Linear may rotate the refresh token
		t.st.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		t.st.Expires = time.Now().Unix() + tok.ExpiresIn
	}
	t.persistLocked()
	log.Printf("refreshed access token for workspace %s (expires %d)", t.st.OrgID, t.st.Expires)
	return nil
}

// tokenResponse is the subset of Linear's /oauth/token reply that both grants
// this receiver uses — authorization_code at install, refresh_token after —
// come back with.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// postOAuth performs one form-encoded POST to Linear's token endpoint. Shared
// by the install exchange and the refresh grant, which differ only in the form
// they send.
func (c *client) postOAuth(ctx context.Context, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, linearOAuth, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return tokenResponse{}, fmt.Errorf("oauth %d: %s", resp.StatusCode, out)
	}
	var tok tokenResponse
	if err := json.Unmarshal(out, &tok); err != nil {
		return tokenResponse{}, fmt.Errorf("decode: %w", err)
	}
	if tok.AccessToken == "" {
		return tokenResponse{}, fmt.Errorf("empty access_token in response")
	}
	return tok, nil
}

// installIdentity is whose a freshly minted token turns out to be: which
// workspace authorized, and which app user this agent is inside it.
type installIdentity struct {
	OrgID     string
	OrgName   string
	OrgURLKey string
	AppUserID string
}

// organizationQuery and viewerQuery are deliberately two round trips rather
// than one nested query: the app user id is a nice-to-have (a second key to
// route webhooks by), while the workspace id is the store's key and must not
// be lost because the other half of a combined query didn't resolve.
const (
	organizationQuery = `query { organization { id name urlKey } }`
	viewerQuery       = `query { viewer { id } }`
)

// identify asks Linear who a just-minted token belongs to. The workspace is
// required — it's what the credentials get filed under. The app user id is
// best-effort: without it a webhook can still be routed by organizationId.
func (c *client) identify(ctx context.Context, token string) (installIdentity, error) {
	out, err := c.doGraphQL(ctx, token, organizationQuery, nil)
	if err != nil {
		return installIdentity{}, fmt.Errorf("fetch organization: %w", err)
	}
	var org struct {
		Data struct {
			Organization struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				URLKey string `json:"urlKey"`
			} `json:"organization"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &org); err != nil {
		return installIdentity{}, fmt.Errorf("decode organization: %w", err)
	}
	if org.Data.Organization.ID == "" {
		return installIdentity{}, fmt.Errorf("no organization id in response")
	}

	id := installIdentity{
		OrgID:     org.Data.Organization.ID,
		OrgName:   org.Data.Organization.Name,
		OrgURLKey: org.Data.Organization.URLKey,
	}

	out, err = c.doGraphQL(ctx, token, viewerQuery, nil)
	if err != nil {
		log.Printf("install %s: no app user id (%v) — webhooks will route by organizationId alone", id.OrgID, err)
		return id, nil
	}
	var viewer struct {
		Data struct {
			Viewer struct {
				ID string `json:"id"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &viewer); err != nil {
		log.Printf("install %s: decode viewer: %v", id.OrgID, err)
		return id, nil
	}
	id.AppUserID = viewer.Data.Viewer.ID
	return id, nil
}
