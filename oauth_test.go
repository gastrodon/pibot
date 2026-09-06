package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStateIsOneShotAndExpires(t *testing.T) {
	s := newStateStore()
	v, err := s.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !s.consume(v) {
		t.Fatal("consume(issued) = false, want true")
	}
	if s.consume(v) {
		t.Fatal("consume(issued) twice = true, want false — a state is spent by its first use")
	}
	if s.consume("never-issued") || s.consume("") {
		t.Fatal("consume of an unissued state = true, want false")
	}

	expired, err := s.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	s.mu.Lock()
	s.issued[expired] = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if s.consume(expired) {
		t.Fatal("consume(expired) = true, want false")
	}
}

func TestRedirectURIIsBuiltFromConfig(t *testing.T) {
	c := newClient(config{publicURL: "https://server1.example.ts.net"})
	got, err := c.redirectURI()
	if err != nil {
		t.Fatalf("redirectURI: %v", err)
	}
	if want := "https://server1.example.ts.net/oauth/callback"; got != want {
		t.Fatalf("redirectURI() = %q, want %q", got, want)
	}

	if _, err := newClient(config{}).redirectURI(); err == nil {
		t.Fatal("redirectURI with no public URL = nil error, want a refusal")
	}
}

func TestOAuthStartRequiresTheAdminToken(t *testing.T) {
	c := newClient(config{clientID: "cid", publicURL: "https://x.example"})

	// No admin token loaded at all: the flow is closed, not open.
	rec := httptest.NewRecorder()
	c.handleOAuthStart(rec, httptest.NewRequest(http.MethodGet, oauthStartPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ungated start = %d, want %d — an unconfigured install flow must fail closed", rec.Code, http.StatusServiceUnavailable)
	}

	c.adminToken = "s3cret"
	for _, q := range []string{"", "?token=", "?token=wrong"} {
		rec := httptest.NewRecorder()
		c.handleOAuthStart(rec, httptest.NewRequest(http.MethodGet, oauthStartPath+q, nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("start%q = %d, want %d", q, rec.Code, http.StatusForbidden)
		}
	}
}

func TestOAuthStartRedirectsToLinear(t *testing.T) {
	c := newClient(config{clientID: "cid", publicURL: "https://server1.example.ts.net"})
	c.adminToken = "s3cret"

	rec := httptest.NewRecorder()
	c.handleOAuthStart(rec, httptest.NewRequest(http.MethodGet, oauthStartPath+"?token=s3cret", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("start = %d, want %d", rec.Code, http.StatusFound)
	}

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if base := loc.Scheme + "://" + loc.Host + loc.Path; base != linearAuthorize {
		t.Fatalf("redirected to %q, want %q", base, linearAuthorize)
	}
	q := loc.Query()
	// actor=app and this scope set are what make the install an *agent*
	// install rather than a login as the approving human.
	want := map[string]string{
		"client_id":     "cid",
		"redirect_uri":  "https://server1.example.ts.net/oauth/callback",
		"response_type": "code",
		"scope":         "read,write,app:assignable,app:mentionable",
		"actor":         "app",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Fatalf("authorize %s = %q, want %q", k, q.Get(k), v)
		}
	}
	// The state must be one this process issued, and redeemable exactly once.
	if !c.oauthStates.consume(q.Get("state")) {
		t.Fatal("authorize state was not issued by this process")
	}
}

func TestOAuthCallbackRejectsUnsolicitedRequests(t *testing.T) {
	c := newClient(config{clientID: "cid", publicURL: "https://x.example"})

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"forged state", "?code=abc&state=never-issued", http.StatusBadRequest},
		{"no state", "?code=abc", http.StatusBadRequest},
		{"authorize declined", "?error=access_denied", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c.handleOAuthCallback(rec, httptest.NewRequest(http.MethodGet, oauthCallbackPath+tc.query, nil))
			if rec.Code != tc.want {
				t.Fatalf("callback%s = %d, want %d", tc.query, rec.Code, tc.want)
			}
		})
	}

	// A state is spent by whichever attempt carries it, however that attempt
	// ends — a one-shot state that survives a failure isn't one-shot.
	for _, tail := range []string{"", "&error=access_denied"} {
		state, err := c.oauthStates.issue()
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		rec := httptest.NewRecorder()
		c.handleOAuthCallback(rec, httptest.NewRequest(http.MethodGet, oauthCallbackPath+"?state="+state+tail, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("callback%q = %d, want %d", tail, rec.Code, http.StatusBadRequest)
		}
		if c.oauthStates.consume(state) {
			t.Fatalf("state survived a failed callback%q, want it spent", tail)
		}
	}
}

func TestOrgAllowed(t *testing.T) {
	open := newClient(config{})
	if !open.orgAllowed("anyone-at-all") {
		t.Fatal("an empty allowlist should accept any workspace")
	}

	gated := newClient(config{allowedOrgs: []string{"f9a4dcde-1f1d-43e1-a9c6-dbded1d624b4"}})
	if !gated.orgAllowed("f9a4dcde-1f1d-43e1-a9c6-dbded1d624b4") {
		t.Fatal("expected the listed workspace to be allowed")
	}
	if gated.orgAllowed("some-other-workspace") {
		t.Fatal("expected an unlisted workspace to be refused")
	}
}

func TestLoadAdminTokenMintsAndReusesOne(t *testing.T) {
	dir := t.TempDir()
	c := newClient(config{stateDir: dir})
	c.loadAdminToken()
	if c.adminToken == "" {
		t.Fatal("loadAdminToken minted nothing — a deployed box could never be installed")
	}

	path := filepath.Join(dir, adminTokenFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("admin token file mode = %o, want 600", perm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.TrimSpace(string(b)) != c.adminToken {
		t.Fatal("the token in memory is not the one on disk")
	}

	// A restart must not invalidate an install URL someone already has.
	again := newClient(config{stateDir: dir})
	again.loadAdminToken()
	if again.adminToken != c.adminToken {
		t.Fatal("loadAdminToken minted a second token instead of reusing the stored one")
	}

	// An explicitly configured token wins and nothing is minted.
	configured := newClient(config{stateDir: t.TempDir(), adminToken: "from-config"})
	configured.loadAdminToken()
	if configured.adminToken != "from-config" {
		t.Fatalf("adminToken = %q, want the configured value", configured.adminToken)
	}
	if _, err := os.Stat(filepath.Join(configured.cfg.stateDir, adminTokenFile)); !os.IsNotExist(err) {
		t.Fatal("a configured admin token should not also mint a file")
	}
}
