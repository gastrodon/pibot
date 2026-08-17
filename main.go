// linear-agent: webhook receiver that bridges Linear agent sessions to Nomad.
//
// On a Linear AgentSessionEvent it (1) verifies the HMAC signature, (2) returns
// 200 within Linear's 5s budget, then asynchronously (3) posts a `thought`
// activity to acknowledge the session within the 10s budget and (4) dispatches
// a parameterized Nomad batch job to run the actual agent (pi) in isolation.
//
// Linear's OAuth access token is short-lived (~24h), so the receiver refreshes
// it in place using the refresh token + client credentials and persists the
// rotated material to STATE_DIR so it survives restarts.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	linearGraphQL = "https://api.linear.app/graphql"
	linearOAuth   = "https://api.linear.app/oauth/token"
)

type config struct {
	listenAddr      string
	webhookSecret   []byte
	refreshToken    string
	clientID        string
	clientSecret    string
	stateDir        string
	nomadAddr       string
	nomadToken      string
	nomadJob        string
	defaultModel    string
	defaultThinking string
	// allowedModels validates a directive-supplied model before dispatch; empty
	// means skip validation. See modelAllowed.
	allowedModels []string
}

func loadConfig() config {
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	// secretOrFile prefers <KEY>_FILE (a path to a file holding the value — the
	// shape sops-nix decrypts secrets into) over the bare <KEY> env var, so the
	// process itself never needs the secret material baked into its environment
	// by whoever wires it up; callers that just have a plain env var still work.
	secretOrFile := func(k string) string {
		if p := os.Getenv(k + "_FILE"); p != "" {
			b, err := os.ReadFile(p)
			if err != nil {
				log.Fatalf("read %s_FILE %q: %v", k, p, err)
			}
			return strings.TrimSpace(string(b))
		}
		return os.Getenv(k)
	}
	// Linear creds are optional at startup: the process stays up (and /health
	// serves) before the Linear app exists. Webhooks are rejected until the
	// signing secret is set; see handleWebhook.
	return config{
		listenAddr:      get("LISTEN_ADDR", ":3456"),
		webhookSecret:   []byte(secretOrFile("LINEAR_WEBHOOK_SECRET")),
		refreshToken:    secretOrFile("LINEAR_REFRESH_TOKEN"),
		clientID:        secretOrFile("LINEAR_CLIENT_ID"),
		clientSecret:    secretOrFile("LINEAR_CLIENT_SECRET"),
		stateDir:        os.Getenv("STATE_DIR"),
		nomadAddr:       get("NOMAD_ADDR", "http://127.0.0.1:4646"),
		nomadToken:      secretOrFile("NOMAD_TOKEN"),
		nomadJob:        get("NOMAD_JOB", "pi-agent"),
		defaultModel:    get("DEFAULT_MODEL", "anthropic/claude-sonnet-5"),
		defaultThinking: get("DEFAULT_THINKING", "high"),
		allowedModels:   splitNonEmpty(os.Getenv("ALLOWED_MODELS"), ","),
	}
}

// splitNonEmpty splits s on sep, trims whitespace, and drops empty pieces. A
// blank s returns nil.
func splitNonEmpty(s, sep string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// agentSessionEvent is the subset of the Linear webhook we act on. Field
// shapes are verified against Linear's public GraphQL schema (the webhook
// body mirrors AgentSession/Issue/Comment) rather than assumed — notably,
// there is no promptContext field anywhere on AgentSession; the actual
// thread context lives in agentSession.context (an opaque JSON blob with no
// documented shape), and the fields worth building a prompt from are
// agentSession.issue.{title,description}, agentSession.summary, and the
// single triggering message (see triggerBody).
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
// the accreted thread history. Directives are parsed only from this field.
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

// tokenState is the refreshable OAuth material. Persisted to the state dir so
// rotations survive restarts (Linear may rotate the refresh token on each use).
type tokenState struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Expires      int64  `json:"expires"` // unix seconds; 0 = unknown
}

func main() {
	cfg := loadConfig()
	c := &client{http: &http.Client{Timeout: 10 * time.Second}, cfg: cfg}
	c.loadToken()

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", c.handleWebhook)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok\n")
	})

	log.Printf("linear-agent listening on %s (nomad job %q)", cfg.listenAddr, cfg.nomadJob)
	srv := &http.Server{Addr: cfg.listenAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

type client struct {
	http *http.Client
	cfg  config
	mu   sync.Mutex // guards tok
	tok  tokenState
}

// loadToken seeds in-memory token state: the persisted state file wins; on first
// run (no file) it falls back to the env-provided token trio and writes the file
// so subsequent refreshes have somewhere to persist. Startup is single-threaded.
func (c *client) loadToken() {
	if c.cfg.stateDir != "" {
		if b, err := os.ReadFile(c.tokenPath()); err == nil {
			var ts tokenState
			if json.Unmarshal(b, &ts) == nil && ts.AccessToken != "" {
				c.tok = ts
				log.Printf("loaded token state from %s (expires %d)", c.tokenPath(), ts.Expires)
				return
			}
		}
	}
	c.tok = tokenState{RefreshToken: c.cfg.refreshToken}
	c.persist(c.tok)
}

func (c *client) tokenPath() string { return filepath.Join(c.cfg.stateDir, "token.json") }

// persist atomically writes token state to the state dir (temp + rename).
func (c *client) persist(ts tokenState) {
	if c.cfg.stateDir == "" {
		return
	}
	b, _ := json.Marshal(ts)
	tmp := c.tokenPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("persist token: %v", err)
		return
	}
	if err := os.Rename(tmp, c.tokenPath()); err != nil {
		log.Printf("persist token rename: %v", err)
	}
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

	if err := c.postActivity(ctx, ev.AgentSession.ID, "thought", "Picking this up — spinning up an agent."); err != nil {
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
	payload, err := json.Marshal(map[string]string{"prompt": buildPrompt(ev)})
	if err != nil {
		return fmt.Errorf("marshal dispatch payload: %w", err)
	}
	if len(payload) > maxDispatchPayload {
		// Defensive only: buildPrompt already caps every field it draws from, so
		// this should be unreachable outside pathological input. Nomad hard-rejects
		// anything over 16KiB, so never send a dispatch un-capped regardless.
		prompt := clip(buildPrompt(ev), maxDispatchPayload-256)
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

// maxDispatchPayload leaves headroom under Nomad's hard-coded 16384-byte
// dispatch payload limit (DispatchPayloadSizeLimit in nomad/job_endpoint.go —
// not a server setting, so we can't raise it).
const maxDispatchPayload = 15 * 1024

// maxFieldBytes bounds any single context component buildPrompt draws from
// (issue description, session summary) so a single oversized field can't push
// the dispatch over Nomad's limit. The triggering message itself (the actual
// instruction) is never capped here — see buildPrompt.
const maxFieldBytes = 4000

// truncatedMarker prefixes any field clip had to cut down, so pi (and anyone
// reading pi.jsonl) can tell the context it saw was partial.
const truncatedMarker = "…[truncated by pibot: content exceeded the dispatch size budget]…"

// clip truncates s to at most n bytes, keeping the tail — Linear (and most
// threaded UIs) assembles context oldest-first, so the newest/most relevant
// text survives a cut. A no-op if s already fits.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return truncatedMarker + trimToUTF8Boundary(s, len(s)-n)
}

// buildPrompt assembles the text pi actually runs on from the fields the
// receiver knows exist on Linear's AgentSessionEvent webhook —
// agentSession.issue.{title,description}, agentSession.summary, and the
// single triggering message (see triggerBody) — instead of forwarding
// Linear's full webhook body and hoping the worker can find a promptContext
// field in it. It can't: Linear's AgentSession GraphQL type, which the
// webhook mirrors, has no such field. Previously that meant every dispatch's
// prompt-extraction fell through to a raw `tojson` of the (possibly
// mid-string-truncated) whole webhook, which is what pi actually ran on —
// unparseable JSON fragments instead of an instruction.
func buildPrompt(ev agentSessionEvent) string {
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
	if body := ev.triggerBody(); body != "" {
		b.WriteString("## Request\n" + body)
	}
	return b.String()
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
