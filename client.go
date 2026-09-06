package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// client is the shared receiver state: an HTTP client, static config, the
// tenant store, and the in-memory index of the workspaces installed into it.
//
// Nothing here is workspace-specific. The Linear OAuth app's client
// id/secret and webhook signing secret are app-level and live in cfg; every
// credential that belongs to one workspace lives on a tenant.
type client struct {
	http  *http.Client
	cfg   config
	store *store

	// adminToken gates /oauth/start. Empty means the install flow is closed.
	// See (*client).loadAdminToken.
	adminToken string
	// oauthStates holds the one-shot CSRF states /oauth/start has issued.
	oauthStates *stateStore

	// mu guards the indexes below, which point at the same tenants under each
	// of the two keys an inbound webhook might identify a workspace by.
	// appUserOf remembers which app user id each workspace is currently
	// indexed under, so a reinstall can drop the stale entry without having
	// to read a tenant's state — reading that would mean taking a tenant's
	// lock under this one, and a token refresh holds a tenant's lock across a
	// network round trip.
	mu        sync.RWMutex
	byOrg     map[string]*tenant
	byAppUser map[string]*tenant
	appUserOf map[string]string
}

// tenant is one installed workspace: its persisted state plus the lock that
// serializes token refreshes for it. The lock is per-tenant, so one
// workspace's expired token can't stall another workspace's dispatch.
type tenant struct {
	c *client
	// org is the workspace this tenant is — the store's key, and immutable
	// for the tenant's lifetime, so it can be read without taking mu.
	org string
	mu  sync.Mutex // guards st
	st  tenantState
}

func newClient(cfg config) *client {
	dir := ""
	if cfg.stateDir != "" {
		dir = filepath.Join(cfg.stateDir, "tenants")
	}
	return &client{
		http:        &http.Client{Timeout: 10 * time.Second},
		cfg:         cfg,
		store:       &store{dir: dir},
		oauthStates: newStateStore(),
		byOrg:       map[string]*tenant{},
		byAppUser:   map[string]*tenant{},
		appUserOf:   map[string]string{},
	}
}

// loadTenants seeds the indexes from the store.
//
// Deliberately never fatal. This runs under Restart=always, and a receiver
// that refuses to start is a receiver whose install endpoint never binds —
// the one thing that could fix a broken store. So it loads what it can, says
// loudly what it couldn't, and comes up; every event for a workspace it failed
// to load then fails visibly at tenantFor rather than silently.
func (c *client) loadTenants() {
	found, bad, err := c.store.loadAll()
	if err != nil {
		log.Printf("WARNING: cannot read the tenant store at %s: %v — no workspace will be served until this is fixed", c.store.dir, err)
	}
	for _, e := range bad {
		log.Printf("WARNING: skipping an unreadable install in %s: %v", c.store.dir, e)
	}
	for _, ts := range found {
		c.putTenant(ts)
		log.Printf("installed workspace %s (%s), token expires %d", ts.OrgID, ts.OrgName, ts.Expires)
	}
	if len(found) == 0 {
		log.Printf("no workspace installed — run the install flow at %s%s", c.cfg.publicURL, oauthStartPath)
		c.warnLegacyToken()
	}
}

// warnLegacyToken points at credentials left by the version of this receiver
// that held one workspace's token in a single token.json, seeded out-of-band.
// Those aren't migrated: the file doesn't record which workspace it belongs
// to, and re-running the install flow settles that in one browser visit. The
// point of saying so is that "no workspace installed" is otherwise a
// bewildering thing to read on a box that was working yesterday.
func (c *client) warnLegacyToken() {
	if c.cfg.stateDir == "" {
		return
	}
	legacy := filepath.Join(c.cfg.stateDir, "token.json")
	if _, err := os.Stat(legacy); err != nil {
		return
	}
	log.Printf("NOTE: %s is left over from the pre-OAuth-install layout and is not used. "+
		"Run the install flow, then delete it — it still holds a refresh token.", legacy)
}

// putTenant indexes ts under both of its keys, replacing whatever was held for
// the same workspace, and returns the live tenant.
//
// The tenant's own lock is taken only after c.mu is released. A token refresh
// holds a tenant's lock across a network round trip, so blocking the index's
// write lock behind one would stall routing for every workspace — and Go's
// RWMutex parks new readers behind a waiting writer, so that stall would reach
// the webhook handler's ack.
func (c *client) putTenant(ts tenantState) *tenant {
	c.mu.Lock()
	t, known := c.byOrg[ts.OrgID]
	if !known {
		// State is set before the tenant is reachable, so a brand-new tenant
		// never needs its lock here at all.
		t = &tenant{c: c, org: ts.OrgID, st: ts}
		c.byOrg[ts.OrgID] = t
	}
	if prev := c.appUserOf[ts.OrgID]; prev != "" && prev != ts.AppUserID {
		delete(c.byAppUser, prev)
	}
	if ts.AppUserID != "" {
		c.byAppUser[ts.AppUserID] = t
		c.appUserOf[ts.OrgID] = ts.AppUserID
	} else {
		delete(c.appUserOf, ts.OrgID)
	}
	c.mu.Unlock()

	if known {
		// A reinstall of a workspace we already hold: swap its credentials
		// under its own lock, outside c.mu.
		t.mu.Lock()
		t.st = ts
		t.mu.Unlock()
	}
	return t
}

// saveTenant indexes an install and writes it through to the store, holding
// the tenant's lock across the write so it can't interleave with a refresh
// persisting the same workspace.
//
// Indexing first is deliberate: if the write fails, the freshly minted token
// is still live in memory and this process can use it, which beats discarding
// a credential we just spent an authorization code on.
func (c *client) saveTenant(ts tenantState) (*tenant, error) {
	t := c.putTenant(ts)
	t.mu.Lock()
	defer t.mu.Unlock()
	return t, c.store.save(t.st)
}

// tenantFor routes an inbound webhook to the workspace that installed us.
//
// organizationId is the reliable key when Linear sends it, but Linear
// documents that field for data-change events and doesn't promise it on agent
// session events — appUserId is the per-workspace identity their agent docs
// tell you to store, so it's the second key.
//
// The allowlist is re-checked here, not just at install: the webhook signing
// secret belongs to the Linear *app*, so any workspace that completes consent
// — including one whose install this receiver refused — gets its events
// delivered here with a valid signature. install() can't refuse before the
// code exchange (it has to hold a token to ask Linear who consented), so this
// is where a refused workspace is actually kept out.
//
// A workspace that is named but not held is refused outright, never guessed
// past: falling through to another install would hand that install's live
// access token, and a prompt built from the naming workspace's own webhook
// body, to the worker.
//
// The sole-install fallback survives only where the payload genuinely couldn't
// be matched — no organizationId, and either no appUserId or an install whose
// app user id was never recorded (identify's viewer query is best-effort). An
// appUserId that misses an install we *do* have the app user id for is a
// foreign workspace, not an unlabelled one, and is refused like any other.
func (c *client) tenantFor(orgID, appUserID string) (*tenant, error) {
	t, err := c.routeTenant(orgID, appUserID)
	if err != nil {
		return nil, err
	}
	// One check, after routing rather than before, so it covers every way a
	// tenant can be arrived at — named, matched by app user, or fallen back
	// to. A workspace dropped from the allowlist after it installed stops
	// being served here, without needing its stored credentials touched.
	if !c.orgAllowed(t.org) {
		return nil, fmt.Errorf("workspace %s is not on this receiver's allowlist", t.org)
	}
	return t, nil
}

// routeTenant resolves the workspace an event belongs to. See tenantFor, which
// wraps it with the allowlist.
func (c *client) routeTenant(orgID, appUserID string) (*tenant, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if orgID != "" {
		if t := c.byOrg[orgID]; t != nil {
			return t, nil
		}
		return nil, fmt.Errorf("no install for organizationId %q", orgID)
	}
	if appUserID != "" {
		if t := c.byAppUser[appUserID]; t != nil {
			return t, nil
		}
	}
	if len(c.byOrg) == 0 {
		return nil, fmt.Errorf("no workspace is installed — run the install flow at %s%s", c.cfg.publicURL, oauthStartPath)
	}
	if len(c.byOrg) == 1 {
		for org, t := range c.byOrg {
			if appUserID != "" && c.appUserOf[org] != "" {
				return nil, fmt.Errorf("no install for appUserId %q (the one installed workspace is a different app user)", appUserID)
			}
			log.Printf("payload identified no workspace (appUserId=%q); falling back to the only install, %s", appUserID, org)
			return t, nil
		}
	}
	return nil, fmt.Errorf("payload identified no installed workspace (appUserId=%q) and %d are installed", appUserID, len(c.byOrg))
}

// persistLocked writes the tenant's current state through to the store. Caller
// must hold t.mu. A failure is logged, not returned: the in-memory token is
// still usable for this dispatch, and losing the rotation only costs a refresh
// after the next restart.
func (t *tenant) persistLocked() {
	if err := t.c.store.save(t.st); err != nil {
		log.Printf("persist workspace %s: %v", t.st.OrgID, err)
	}
}
