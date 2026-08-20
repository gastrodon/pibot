# pibot

Linear agent-session webhook receiver + isolated headless [pi](https://github.com/earendil-works/pi)
worker, extracted from [`gastrodon/dotfiles`](https://github.com/gastrodon/dotfiles).

## Pieces

- **`*.go`** (`linear-agent`) — HTTP receiver for Linear's `AgentSessionEvent`
  webhooks. Verifies the HMAC signature, acks the session with a `thought`
  activity, and dispatches a parameterized Nomad batch job to run the actual
  agent in isolation. Refreshes its own Linear OAuth access token in place.
  Split by concern: `config.go` (env config), `client.go` (shared client +
  persisted OAuth state), `linear.go` (Linear GraphQL API + token refresh),
  `webhook.go` (HTTP handler), `nomad.go` (job dispatch), `payload.go`
  (shrinking oversized webhook payloads to fit Nomad's dispatch limit).
- **`mint-token.py`** — one-shot OAuth helper to mint the Linear app's initial
  refresh token.
- **`module/linear-agent.nix`** — NixOS module: builds and runs the receiver as
  a systemd service.
- **`module/pi-agent.nix`** — NixOS module: the parameterized Nomad job spec
  (`pi-agent`) the receiver dispatches per session, plus the Nix-built runtime
  image and entrypoint that runs `pi` headlessly (RPC mode, to agent_end) and
  posts its output back to Linear. The image ships `nix` (`nix-command` +
  `flakes` enabled, sandbox off — the podman task is unprivileged) so pibot
  can build/test [`gastrodon/dotfiles`](https://github.com/gastrodon/dotfiles)
  changes the same way its CI does, e.g. `nix build
  .#nixosConfigurations.<host>.config.system.build.toplevel --impure`, and
  `go` so pibot can `go build`/`go test`/`go vet` when it's dispatched to work
  in a Go repo (including its own, `gastrodon/pibot`). Nix
  store state isn't persisted across dispatches, and dotfiles' `free-code` and
  `ifunny-re` flake inputs are private repos fetched over `git+ssh` — pibot has
  no SSH key, so a build touching those inputs will fail to fetch them until
  that's resolved. It also carries `psyduck` (`gastrodon/psyduck`, pulled in as
  a flake input and built the same as any other package) and Playwright
  (`/opt/playwright`, `NODE_PATH`/`PLAYWRIGHT_BROWSERS_PATH` pointed at it) so
  pibot can drive real psyduck pipelines — e.g. running a registry repo's own
  `playwright-navigate` validation check — without fetching either toolchain
  per dispatch.
- **`module/pi-agent-system-prompt.md`** — the worker's operating manual, baked
  into the runtime image and passed as `--append-system-prompt`. Linear's
  workspace/team agent guidance is appended per-dispatch.

## Secrets

This repo owns **no secrets**. Every module option that needs one is a
`*File` path (`webhookSecretFile`, `refreshTokenFile`, `clientIdFile`,
`clientSecretFile`, `nomadTokenFile`, `githubPatFile`,
`nomadBootstrapTokenFile`, `authFile`) — `config.go` reads `<KEY>_FILE` in preference to a
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
            authFile = /* ... */;
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

**Every node carrying that meta needs credentials.** The job is placed on any
of them, so one unseeded node silently swallows a share of all dispatches:
`pi` rejects the prompt, then idles until the run is killed at
`timeoutSeconds`. Set `authFile` and a new node seeds itself on its first
dispatch (the volume copy wins ever after, since `pi` rotates it); leave it
unset and such a node reports the problem on the Linear thread instead of
failing quietly.

By default the worker routes to Anthropic (`claude-sonnet-5`, high thinking)
via `pi-black`. To route it at a local Ollama endpoint instead, declare the
provider *and* point the fleet defaults at it:

```nix
services.piAgent = {
  ollama = {
    enable = true;
    baseUrl = "http://ollama.example:11434/v1";
    model = "qwen2.5-coder:7b";
  };

  # what every dispatch actually runs
  provider = "ollama";
  model = "qwen2.5-coder:7b";
  thinkingLevel = "off";
};
```

This bakes a `models.json` (`api = "openai-completions"`, `apiKey = "ollama"`,
developer-role and reasoning-effort compat both off) alongside `settings.json`,
copied onto the persistent volume every dispatch (and removed from the volume
if `ollama.enable` is later flipped off, so a stale file never points at a
dead endpoint). An ollama-routed worker runs exactly one model, hence a single
required `model` rather than a list.

`provider`/`model`/`thinkingLevel` set the fleet-wide default. `linear-agent`'s
`dispatchNomad` (`nomad.go`) always sends `model`/`thinking` dispatch Meta,
resolved by `webhook.go`'s `resolveRouting` and defaulting to
`services.linearAgent.defaultModel`/`defaultThinking` (which should match the
fleet default above) — the entrypoint reads those as
`NOMAD_META_model`/`NOMAD_META_thinking` and passes them to `pi` as
`--model`/`--thinking`.

A Linear commenter can override either per session with a trailing directive
line on their comment or prompt:

```
pibot: model=ollama/qwen2.5-coder:7b thinking=off
```

Only the single triggering message is parsed (the initiating comment, or the
latest prompt on a follow-up) — never the accumulated thread history — and
only its last line, so it can't be mistaken for prose earlier in the body. A
requested model that isn't in `services.linearAgent.allowedModels` is
rejected with an `error` activity on the thread instead of being dispatched;
the list is empty (validation off, any directive dispatches) until there's a
real roster worth enforcing. `settings.json`'s
`defaultProvider`/`defaultModel`/`defaultThinkingLevel` only matter as the
fallback for dispatches with no Meta at all (e.g. a manual `nomad job dispatch`
with `model`/`thinking` omitted).

Note that a default model configured on the Ollama server itself has no effect
here: pi's OpenAI-completions requests always name a model explicitly, so the
choice has to come from the `model` dispatch Meta (or, absent that,
`settings.json`).
