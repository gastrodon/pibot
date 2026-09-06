// linear-agent: webhook receiver that bridges Linear agent sessions to Nomad.
//
// On a Linear AgentSessionEvent it (1) verifies the HMAC signature, (2) returns
// 200 within Linear's 5s budget, then asynchronously (3) posts a `thought`
// activity to acknowledge the session within the 10s budget and (4) dispatches
// a parameterized Nomad batch job to run the actual agent (pi) in isolation.
//
// The receiver holds no Linear credentials of its own until a workspace is
// installed into it: /oauth/start → Linear consent → /oauth/callback mints an
// access + refresh token and files it under that workspace in STATE_DIR. So a
// box can be deployed first and authorized after. Access tokens are
// short-lived (~24h), so the receiver refreshes them in place per workspace
// and persists the rotated material, which is what survives a restart.
//
// The receiver's code is split by concern:
//
//   - config.go   — env-derived configuration.
//   - client.go   — the shared client, and the per-workspace tenants it routes
//     inbound events to.
//   - store.go    — the on-disk token store: one file per installed workspace.
//   - oauth.go    — the install flow: authorize, callback, exchange, persist.
//   - linear.go   — talking to Linear's GraphQL API: activities, token
//     refresh, and resolving a session's trigger comment + thread.
//   - prompt.go   — assembling the system/user prompt from a resolved
//     session context, embedding the static system prompt template.
//   - webhook.go  — the HTTP handler: signature verification and dispatch.
//   - nomad.go    — kicking the Nomad batch job.
package main

import (
	"io"
	"log"
	"net/http"
	"time"
)

func main() {
	cfg := loadConfig()
	c := newClient(cfg)
	c.loadTenants()
	c.loadAdminToken()

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", c.handleWebhook)
	mux.HandleFunc(oauthStartPath, c.handleOAuthStart)
	mux.HandleFunc(oauthCallbackPath, c.handleOAuthCallback)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok\n")
	})

	log.Printf("linear-agent listening on %s (nomad job %q)", cfg.listenAddr, cfg.nomadJob)
	srv := &http.Server{Addr: cfg.listenAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
