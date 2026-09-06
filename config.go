package main

import (
	"log"
	"os"
	"slices"
	"strings"
)

type config struct {
	listenAddr    string
	webhookSecret []byte
	clientID      string
	clientSecret  string
	// publicURL is the external base URL a tunnel fronts this receiver at. It
	// is what the OAuth redirect_uri is built from — derived from config, not
	// from an inbound request's Host, because the redirect_uri has to be
	// byte-identical across authorize/exchange and registered on the Linear
	// app. See (*client).redirectURI.
	publicURL string
	// adminToken gates the install flow. Optional: with no value configured
	// the receiver mints its own into stateDir, so a box that has only ever
	// been deployed can still be installed. See (*client).loadAdminToken.
	adminToken string
	// allowedOrgs is the workspace allowlist an install must satisfy before
	// its credentials are persisted; empty means any workspace may install.
	// See (*client).orgAllowed.
	allowedOrgs     []string
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
	// serves) before the Linear app exists, and holds no workspace token at
	// all until someone runs the install flow against it. Webhooks are
	// rejected until the signing secret is set; see handleWebhook.
	return config{
		listenAddr:      get("LISTEN_ADDR", ":3456"),
		webhookSecret:   []byte(secretOrFile("LINEAR_WEBHOOK_SECRET")),
		clientID:        secretOrFile("LINEAR_CLIENT_ID"),
		clientSecret:    secretOrFile("LINEAR_CLIENT_SECRET"),
		publicURL:       strings.TrimRight(os.Getenv("PUBLIC_URL"), "/"),
		adminToken:      secretOrFile("ADMIN_TOKEN"),
		allowedOrgs:     splitNonEmpty(os.Getenv("ALLOWED_ORGS"), ","),
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

// allowed reports whether v is in list, treating an empty list as "no
// allowlist configured" and accepting anything. Both allowlists this receiver
// carries — reachable models and installable workspaces — are that shape.
func allowed(list []string, v string) bool {
	return len(list) == 0 || slices.Contains(list, v)
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
