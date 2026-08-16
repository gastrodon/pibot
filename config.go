package main

import (
	"log"
	"os"
	"strings"
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
	// means skip validation. See (*client).modelAllowed.
	allowedModels []string
}

func loadConfig() config {
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
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

// secretOrFile prefers <KEY>_FILE (a path to a file holding the value — the
// shape sops-nix decrypts secrets into) over the bare <KEY> env var, so the
// process itself never needs the secret material baked into its environment
// by whoever wires it up; callers that just have a plain env var still work.
func secretOrFile(k string) string {
	if p := os.Getenv(k + "_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			log.Fatalf("read %s_FILE %q: %v", k, p, err)
		}
		return strings.TrimSpace(string(b))
	}
	return os.Getenv(k)
}
