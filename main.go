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
//
// The receiver's code is split by concern:
//
//   - config.go   — env-derived configuration.
//   - client.go   — the shared client struct and its persisted OAuth state.
//   - linear.go   — talking to Linear's GraphQL API (activities, token refresh,
//     fetching thread context).
//   - webhook.go  — the HTTP handler: signature verification and dispatch.
//   - nomad.go    — kicking the Nomad batch job.
//   - prompt.go   — assembling the dispatch prompt from the webhook event and
//     fetched thread context.
package main

import (
	"io"
	"log"
	"net/http"
	"time"
)

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
