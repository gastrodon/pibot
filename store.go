package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// tenantState is one Linear workspace's install: the identity an inbound
// webhook is routed back to it by, plus the refreshable OAuth material minted
// for it. The Linear OAuth *app* (client id/secret, webhook signing secret)
// stays shared and comes from config — only what's below is per-install, which
// is what makes one deployed receiver able to serve more than one workspace.
type tenantState struct {
	// OrgID is the Linear organization (workspace) id. It's the store's key,
	// the filename, and the field an AgentSessionEvent's organizationId
	// matches on.
	OrgID string `json:"organization_id"`
	// OrgName / OrgURLKey are cosmetic — they make a state directory readable
	// and let the install page name the workspace it just linked.
	OrgName   string `json:"organization_name,omitempty"`
	OrgURLKey string `json:"organization_url_key,omitempty"`
	// AppUserID is this app's `viewer.id` inside that workspace: unique per
	// workspace, and the identity Linear's own agent docs recommend storing
	// alongside the token. It backs tenant resolution for payload shapes that
	// carry appUserId but no organizationId.
	AppUserID string `json:"app_user_id,omitempty"`

	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Expires      int64  `json:"expires"` // unix seconds; 0 = unknown
}

// store persists tenantState as one file per workspace under dir. A directory
// of small files, rather than a single blob, so an install or a token rotation
// only ever rewrites its own workspace's credentials.
//
// Writes are serialized on mu. There is more than one writer — a completing
// install and a token refresh for the same workspace can land together — and
// two unsynchronized temp-file writes to the same path interleave into a torn
// file, which the next start would then refuse to decode.
type store struct {
	mu  sync.Mutex
	dir string
}

// safeKey reports whether k can be used verbatim as a filename. Workspace ids
// are UUIDs, so this is defence against a malformed identifier reaching the
// filesystem — not a transformation: a key that fails is rejected, never
// silently rewritten into a different one.
func safeKey(k string) bool {
	if k == "" {
		return false
	}
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func (s *store) path(orgID string) string { return filepath.Join(s.dir, orgID+".json") }

// loadAll reads every install in the store, ordered by workspace id so startup
// is deterministic, and separately reports the files it could not read.
//
// One bad file does not sink the rest, and never fails the load: this runs at
// startup under Restart=always, and a single corrupt tenant file that aborted
// startup would crash-loop the process — taking down the very install endpoint
// you'd reinstall through. A missing directory isn't a failure either; it's
// what a freshly-deployed, not-yet-installed receiver looks like.
func (s *store) loadAll() (found []tenantState, bad []error, err error) {
	if s.dir == "" {
		return nil, nil, nil
	}
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", s.dir, err)
	}

	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			bad = append(bad, fmt.Errorf("read %s: %w", e.Name(), err))
			continue
		}
		var ts tenantState
		if err := json.Unmarshal(b, &ts); err != nil {
			bad = append(bad, fmt.Errorf("decode %s: %w", e.Name(), err))
			continue
		}
		if ts.OrgID == "" {
			bad = append(bad, fmt.Errorf("decode %s: no organization_id", e.Name()))
			continue
		}
		found = append(found, ts)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].OrgID < found[j].OrgID })
	return found, bad, nil
}

// save atomically writes one workspace's state (unique temp + rename, 0600),
// serialized against every other save. An unset dir is a hard error rather
// than a silent no-op: the caller that matters is a completed install, and
// dropping a freshly minted credential on the floor would look like success.
func (s *store) save(ts tenantState) error {
	if s.dir == "" {
		return fmt.Errorf("no state directory configured — set STATE_DIR")
	}
	if !safeKey(ts.OrgID) {
		return fmt.Errorf("refusing to store workspace id %q: not a plain identifier", ts.OrgID)
	}
	b, err := json.Marshal(ts)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir, err)
	}
	// A per-call temp name, not "<org>.json.tmp": even serialized, a fixed
	// name is a landmine for any future writer that forgets the lock.
	tmp, err := os.CreateTemp(s.dir, ts.OrgID+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", s.dir, err)
	}
	defer os.Remove(tmp.Name()) // no-op once the rename below succeeds
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmp.Name(), err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), s.path(ts.OrgID)); err != nil {
		return fmt.Errorf("rename %s: %w", tmp.Name(), err)
	}
	return nil
}
