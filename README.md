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
