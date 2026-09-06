package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The OAuth install flow — what mint-token.py used to do out-of-band, folded
// into the running receiver so a deployment can be authorized *after* it
// exists instead of needing a token minted before it does. Two endpoints:
//
//	GET /oauth/start?token=<admin>   → 302 to Linear's consent screen
//	GET /oauth/callback?code&state   → exchange, identify, persist
//
// They share the listener the webhook uses, which a tunnel fronts publicly
// (Tailscale Funnel, in this deployment) — the callback has to be publicly
// reachable, because it's the browser Linear redirects. So both are gated:
//
//   - /oauth/start needs the admin token, so a stranger can't start an install
//     against this receiver at all.
//   - /oauth/callback only honours a `state` this process issued, once, within
//     a short TTL — and even a completed consent is refused if the workspace
//     isn't on the allowlist. The allowlist is the real boundary: it's checked
//     against who Linear says authorized, not against who asked.

const (
	linearAuthorize = "https://linear.app/oauth/authorize"

	oauthStartPath    = "/oauth/start"
	oauthCallbackPath = "/oauth/callback"

	// oauthScope is the same set mint-token.py requested. actor=app installs
	// pibot as an app user — assignable and mentionable in its own right —
	// rather than acting as the human who approved it.
	oauthScope = "read,write,app:assignable,app:mentionable"
	oauthActor = "app"

	// oauthStateTTL bounds how long an issued state stays redeemable. Long
	// enough to read a consent screen, short enough that an abandoned start
	// isn't left standing.
	oauthStateTTL = 10 * time.Minute

	adminTokenFile = "admin-token"
)

// stateStore holds the OAuth `state` values this process has issued but not
// yet seen come back. A callback is honoured only for a state we minted, and
// only once — that's what stops a stranger replaying a code, or steering a
// consent we never initiated into our token store.
type stateStore struct {
	mu     sync.Mutex
	issued map[string]time.Time // state → expiry
}

func newStateStore() *stateStore { return &stateStore{issued: map[string]time.Time{}} }

func (s *stateStore) issue() (string, error) {
	v, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.issued[v] = time.Now().Add(oauthStateTTL)
	return v, nil
}

// consume redeems v, reporting whether it was outstanding and unexpired. The
// delete happens either way, so a state is spent by the first attempt to use
// it whether or not that attempt succeeded.
func (s *stateStore) consume(v string) bool {
	if v == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	expires, ok := s.issued[v]
	delete(s.issued, v)
	return ok && time.Now().Before(expires)
}

// sweepLocked drops expired states. There's no background timer — the map only
// grows when someone hits /oauth/start, which is gated, so sweeping on each
// access is enough to keep it bounded. Caller must hold s.mu.
func (s *stateStore) sweepLocked() {
	now := time.Now()
	for v, expires := range s.issued {
		if now.After(expires) {
			delete(s.issued, v)
		}
	}
}

// randomToken returns 32 bytes of crypto-random entropy, URL-safe.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// loadAdminToken settles what gates /oauth/start.
//
// A configured token wins. The fallback is the point of the whole exercise: a
// box that has only ever been deployed — never hand-seeded with secret
// material — mints its own token into the state dir on first boot, so whoever
// can read that file can install a workspace into it. Deploy first, authorize
// after. The token is never logged; read it off the box.
func (c *client) loadAdminToken() {
	if c.cfg.adminToken != "" {
		c.adminToken = c.cfg.adminToken
		log.Printf("install flow gated by the configured admin token")
		return
	}
	if c.cfg.stateDir == "" {
		log.Printf("no admin token and no STATE_DIR: %s is closed", oauthStartPath)
		return
	}

	path := filepath.Join(c.cfg.stateDir, adminTokenFile)
	if b, err := os.ReadFile(path); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			c.adminToken = v
			log.Printf("install flow gated by the token in %s", path)
			return
		}
	}

	v, err := randomToken()
	if err != nil {
		log.Printf("generate admin token: %v — %s is closed", err, oauthStartPath)
		return
	}
	// systemd's StateDirectory= creates this, but nothing else in the process
	// guarantees it, and a missing directory here would close the install flow
	// for the life of the process.
	if err := os.MkdirAll(c.cfg.stateDir, 0o700); err != nil {
		log.Printf("mkdir %s: %v — %s is closed", c.cfg.stateDir, err, oauthStartPath)
		return
	}
	if err := os.WriteFile(path, []byte(v+"\n"), 0o600); err != nil {
		log.Printf("write %s: %v — %s is closed", path, err, oauthStartPath)
		return
	}
	c.adminToken = v
	log.Printf("minted an admin token into %s — install with %s%s?token=$(cat %s)",
		path, c.cfg.publicURL, oauthStartPath, path)
}

// orgAllowed reports whether a workspace may complete an install. An empty
// allowlist is the public multi-tenant posture and accepts any workspace; a
// populated one is what keeps a privately-hosted receiver from being installed
// into a stranger's workspace by anyone who gets past /oauth/start.
func (c *client) orgAllowed(org string) bool { return allowed(c.cfg.allowedOrgs, org) }

// redirectURI is the callback Linear sends the browser back to. It has to be
// byte-identical between the authorize request and the token exchange, and
// registered on the Linear OAuth app — so it comes from one configured public
// base URL, never from the inbound request's Host, which a tunnel can rewrite.
func (c *client) redirectURI() (string, error) {
	if c.cfg.publicURL == "" {
		return "", fmt.Errorf("PUBLIC_URL is not set, so there is no redirect_uri to authorize against")
	}
	return c.cfg.publicURL + oauthCallbackPath, nil
}

func (c *client) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Fail closed: with no admin token there is nothing to check a caller
	// against, and an ungated start is an open invitation to install.
	if c.adminToken == "" {
		http.Error(w, "install flow not configured", http.StatusServiceUnavailable)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(c.adminToken)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if c.cfg.clientID == "" {
		http.Error(w, "no Linear client id configured", http.StatusServiceUnavailable)
		return
	}
	redirect, err := c.redirectURI()
	if err != nil {
		log.Printf("oauth start: %v", err)
		http.Error(w, "receiver has no public URL configured", http.StatusServiceUnavailable)
		return
	}
	state, err := c.oauthStates.issue()
	if err != nil {
		log.Printf("oauth start: issue state: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	q := url.Values{
		"client_id":     {c.cfg.clientID},
		"redirect_uri":  {redirect},
		"response_type": {"code"},
		"scope":         {oauthScope},
		"actor":         {oauthActor},
		"state":         {state},
	}
	http.Redirect(w, r, linearAuthorize+"?"+q.Encode(), http.StatusFound)
}

func (c *client) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	// State first, and unconditionally — before the error branch, so that a
	// callback carrying `error=` still spends the state it was issued. A
	// one-shot state that survives a failed attempt isn't one-shot.
	ok := c.oauthStates.consume(q.Get("state"))
	if e := q.Get("error"); e != "" {
		http.Error(w, "authorize failed: "+e, http.StatusBadRequest)
		return
	}
	if !ok {
		http.Error(w, "unknown or expired state — start the install again", http.StatusBadRequest)
		return
	}
	code := q.Get("code")
	if code == "" {
		http.Error(w, "no code in callback", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	ts, err := c.install(ctx, code)
	if err != nil {
		log.Printf("oauth install: %v", err)
		// The allowlist rejection is the one failure worth spelling out: it
		// tells the caller nothing they didn't already know, and silence
		// there reads as a bug rather than as a policy. Everything else
		// stays generic — this endpoint is public, and the details are
		// about our own configuration.
		var notAllowed errOrgNotAllowed
		if errors.As(err, &notAllowed) {
			http.Error(w, notAllowed.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, "install failed — see the receiver's logs", http.StatusBadGateway)
		return
	}

	name := ts.OrgName
	if name == "" {
		name = ts.OrgID
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "pibot is installed for %s. You can close this tab.\n", name)
}

// errOrgNotAllowed is a consent that completed for a workspace allowedOrgs
// doesn't list. Its credentials are never persisted.
type errOrgNotAllowed struct{ org, name string }

func (e errOrgNotAllowed) Error() string {
	return fmt.Sprintf("workspace %s (%s) is not on this receiver's allowlist", e.name, e.org)
}

// install completes the dance for one workspace: exchange the code for tokens,
// ask Linear whose they are, check that workspace is allowed, persist, index.
// Order matters — identify before persist, so the allowlist is checked against
// Linear's answer rather than against anything the caller supplied.
func (c *client) install(ctx context.Context, code string) (tenantState, error) {
	redirect, err := c.redirectURI()
	if err != nil {
		return tenantState{}, err
	}
	tok, err := c.postOAuth(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {c.cfg.clientID},
		"client_secret": {c.cfg.clientSecret},
	})
	if err != nil {
		return tenantState{}, fmt.Errorf("exchange code: %w", err)
	}

	id, err := c.identify(ctx, tok.AccessToken)
	if err != nil {
		return tenantState{}, fmt.Errorf("identify install: %w", err)
	}
	if !c.orgAllowed(id.OrgID) {
		return tenantState{}, errOrgNotAllowed{org: id.OrgID, name: id.OrgName}
	}

	ts := tenantState{
		OrgID:        id.OrgID,
		OrgName:      id.OrgName,
		OrgURLKey:    id.OrgURLKey,
		AppUserID:    id.AppUserID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
	}
	if tok.ExpiresIn > 0 {
		ts.Expires = time.Now().Unix() + tok.ExpiresIn
	}
	if _, err := c.saveTenant(ts); err != nil {
		return tenantState{}, fmt.Errorf("persist install: %w", err)
	}
	log.Printf("installed workspace %s (%s), app user %s, token expires %d",
		ts.OrgID, ts.OrgName, ts.AppUserID, ts.Expires)
	return ts, nil
}
