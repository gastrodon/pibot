# pibot

Linear agent-session webhook receiver + isolated headless [pi](https://github.com/earendil-works/pi)
worker, extracted from [`gastrodon/dotfiles`](https://github.com/gastrodon/dotfiles).

## Pieces

- **`main.go`** (`linear-agent`) — HTTP receiver for Linear's `AgentSessionEvent`
  webhooks. Verifies the HMAC signature, acks the session with a `thought`
  activity, and dispatches a parameterized Nomad batch job to run the actual
  agent in isolation. Refreshes its own Linear OAuth access token in place.
- **`mint-token.py`** — one-shot OAuth helper to mint the Linear app's initial
  refresh token.
- **`module/linear-agent.nix`** — NixOS module: builds and runs the receiver as
  a systemd service.
- **`module/pi-agent.nix`** — NixOS module: the parameterized Nomad job spec
  (`pi-agent`) the receiver dispatches per session, plus the Nix-built runtime
  image and entrypoint that runs `pi` headlessly (RPC mode, to agent_end) and
  posts its output back to Linear.
- **`module/pi-agent-system-prompt.md`** — the worker's operating manual, baked
  into the runtime image and passed as `--append-system-prompt`. Linear's
  workspace/team agent guidance is appended per-dispatch.

## Secrets

This repo owns **no secrets**. Every module option that needs one is a
`*File` path (`webhookSecretFile`, `refreshTokenFile`, `clientIdFile`,
`clientSecretFile`, `nomadTokenFile`, `githubPatFile`,
`nomadBootstrapTokenFile`) — `main.go` reads `<KEY>_FILE` in preference to a
bare `<KEY>` env var. The consuming flake is responsible for decrypting
secret material and handing over paths, e.g. sops-nix's
`config.sops.secrets.<name>.path`.

## Usage

Add this flake as an input and import the modules you need:

```nix
{
  inputs.pibot.url = "github:gastrodon/pibot";

  outputs = { pibot, ... }: {
    nixosConfigurations.server = nixpkgs.lib.nixosSystem {
      modules = [
        pibot.nixosModules.linearAgent
        pibot.nixosModules.piAgent
        {
          services.linearAgent = {
            enable = true;
            webhookSecretFile = /* ... */;
            refreshTokenFile = /* ... */;
            clientIdFile = /* ... */;
            clientSecretFile = /* ... */;
            nomadTokenFile = /* ... */;
          };
          services.piAgent = {
            enable = true;
            githubPatFile = /* ... */;
            nomadBootstrapTokenFile = /* ... */;
          };
        }
      ];
    };
  };
}
```

`services.piAgent` expects a Nomad client with `meta.pi_worker = "true"` (see
the `pi_worker` constraint in `module/pi-agent.nix`) and a persistent
`/var/lib/pi-agent/home` volume for `pi`'s auth state.

By default the worker routes to Anthropic (`claude-sonnet-5`, high thinking)
via `pi-black`. To also route requests at a local Ollama endpoint, set
`services.piAgent.ollama`:

```nix
services.piAgent.ollama = {
  enable = true;
  baseUrl = "http://ollama.example:11434/v1";
  model = "qwen2.5-coder:7b";
};
```

This bakes a `models.json` (`api = "openai-completions"`, `apiKey = "ollama"`,
developer-role and reasoning-effort compat both off) alongside `settings.json`,
copied onto the persistent volume every dispatch (and removed from the volume
if `ollama.enable` is later flipped off, so a stale file never points at a
dead endpoint). An ollama-routed worker runs exactly one model, hence a single
required `model` rather than a list.

**Not wired up end to end yet:** the entrypoint's `--model` override reads
`NOMAD_META_model`, but `linear-agent`'s `dispatchNomad` (`main.go`) only ever
sends `session_id`, `action`, and `access_token` as dispatch Meta — it doesn't
send `model`. So today, no Linear-originated session can reach `ollama/<id>`;
only a manual `nomad job dispatch -meta model=ollama/<id> pi-agent` can. Wiring
per-session model selection through the receiver is separate follow-up work.
