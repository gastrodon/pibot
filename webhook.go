package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// agentSessionEvent is the subset of the Linear webhook we act on.
type agentSessionEvent struct {
	Type         string `json:"type"`
	Action       string `json:"action"`
	AgentSession struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
		Comment struct {
			Body string `json:"body"`
		} `json:"comment"`
		Issue struct {
			Identifier  string `json:"identifier"`
			Title       string `json:"title"`
			Description string `json:"description"`
			URL         string `json:"url"`
		} `json:"issue"`
	} `json:"agentSession"`
	AgentActivity struct {
		Content struct {
			Body string `json:"body"`
		} `json:"content"`
	} `json:"agentActivity"`
}

// triggerBody returns the single message that triggered this event — the
// initiating comment on `created`, the latest prompt on `prompted` — never
// the accreted promptContext thread history. Directives are parsed only from
// this field, so they're unaffected by shrinkPayload's truncation.
func (ev agentSessionEvent) triggerBody() string {
	switch ev.Action {
	case "created":
		return ev.AgentSession.Comment.Body
	case "prompted":
		return ev.AgentActivity.Content.Body
	default:
		return ""
	}
}

// directiveLineRe matches a trailing `pibot: key=value ...` directive — only
// the last non-blank line of the triggering message, so it can't be mistaken
// for prose earlier in the body.
var directiveLineRe = regexp.MustCompile(`(?i)^pibot:\s*(\S.*)$`)
var directiveKVRe = regexp.MustCompile(`(\w+)=(\S+)`)

// parseDirective extracts key=value pairs from body's trailing directive
// line. Returns nil if there is none.
func parseDirective(body string) map[string]string {
	lines := strings.Split(strings.TrimRight(body, " \t\r\n"), "\n")
	m := directiveLineRe.FindStringSubmatch(strings.TrimSpace(lines[len(lines)-1]))
	if m == nil {
		return nil
	}
	kv := map[string]string{}
	for _, pair := range directiveKVRe.FindAllStringSubmatch(m[1], -1) {
		kv[strings.ToLower(pair[1])] = pair[2]
	}
	return kv
}

// resolveRouting picks the model/thinking dispatch Meta: a trailing `pibot:`
// directive on the triggering message overrides the configured default.
func (c *client) resolveRouting(ev agentSessionEvent) (model, thinking string) {
	model, thinking = c.cfg.defaultModel, c.cfg.defaultThinking
	d := parseDirective(ev.triggerBody())
	if v := d["model"]; v != "" {
		model = v
	}
	if v := d["thinking"]; v != "" {
		thinking = v
	}
	return model, thinking
}

// modelAllowed reports whether model may be dispatched. An empty allowlist
// (the default) means validation is off.
func (c *client) modelAllowed(model string) bool {
	if len(c.cfg.allowedModels) == 0 {
		return true
	}
	for _, m := range c.cfg.allowedModels {
		if m == model {
			return true
		}
	}
	return false
}

func (c *client) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Fail closed until the Linear signing secret is configured — an empty-key
	// HMAC would be forgeable, so reject rather than verify against "".
	if len(c.cfg.webhookSecret) == 0 {
		http.Error(w, "receiver not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if !c.verify(r.Header.Get("Linear-Signature"), body) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	var ev agentSessionEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	// Ack the HTTP request immediately (5s budget); do the slow work off-thread.
	w.WriteHeader(http.StatusOK)

	if ev.Type != "AgentSessionEvent" || ev.AgentSession.ID == "" {
		return
	}
	go c.dispatch(ev)
}

// verify checks the Linear-Signature header: hex(HMAC-SHA256(rawBody, secret)).
func (c *client) verify(sig string, body []byte) bool {
	if sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, c.cfg.webhookSecret)
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(sig))
}

// dispatch posts the thought ack, then dispatches the Nomad job; on dispatch
// failure it surfaces an error activity back to the session.
func (c *client) dispatch(ev agentSessionEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := c.postActivity(ctx, ev.AgentSession.ID, "thought", ackThought); err != nil {
		log.Printf("thought ack failed for session %s: %v", ev.AgentSession.ID, err)
	}

	model, thinking := c.resolveRouting(ev)
	if !c.modelAllowed(model) {
		msg := fmt.Sprintf("Unknown model %q — not starting an agent. Known models: %s", model, strings.Join(c.cfg.allowedModels, ", "))
		if e := c.postActivity(ctx, ev.AgentSession.ID, "error", msg); e != nil {
			log.Printf("error activity failed for session %s: %v", ev.AgentSession.ID, e)
		}
		return
	}

	if err := c.dispatchNomad(ctx, ev, model, thinking); err != nil {
		log.Printf("nomad dispatch failed for session %s: %v", ev.AgentSession.ID, err)
		msg := fmt.Sprintf("Couldn't start the agent job: %v", err)
		if e := c.postActivity(ctx, ev.AgentSession.ID, "error", msg); e != nil {
			log.Printf("error activity failed for session %s: %v", ev.AgentSession.ID, e)
		}
	}
}
