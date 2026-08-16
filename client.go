package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// client is the shared receiver state: an HTTP client, static config, and the
// mutex-guarded, persisted Linear OAuth token.
type client struct {
	http *http.Client
	cfg  config
	mu   sync.Mutex // guards tok
	tok  tokenState
}

// tokenState is the refreshable OAuth material. Persisted to the state dir so
// rotations survive restarts (Linear may rotate the refresh token on each use).
type tokenState struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Expires      int64  `json:"expires"` // unix seconds; 0 = unknown
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
