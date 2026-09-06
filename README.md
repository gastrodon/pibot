# pibot

Linear agent-session webhook receiver + isolated headless [pi](https://github.com/earendil-works/pi)
worker, extracted from [`gastrodon/dotfiles`](https://github.com/gastrodon/dotfiles).

## Pieces

- **`*.go`** (`linear-agent`) — HTTP receiver for Linear's `AgentSessionEvent`
  webhooks. Verifies the HMAC signature, acks the session with a `thought`
  activity, and dispatches a parameterized Nomad batch job to run the actual
  agent in isolation. Mints and refreshes its own Linear OAuth tokens, one set
  per installed workspace. Split by concern: `config.go` (env config),
  `client.go` (shared client + the per-workspace tenants it routes events to),
  `store.go` (the on-disk token store, one file per workspace), `oauth.go`
  (the install flow: authorize, callback, exchange, persist), `linear.go`
  (Linear GraphQL API: activities, token refresh, and resolving a session's
  trigger comment + thread), `prompt.go` (assembling the system/user prompt
  from that resolved context, so the dispatch payload carries a finished
  `{system, prompt}` object instead of the raw webhook —
  `module/pi-agent.nix`'s entrypoint just writes those two fields out, with no
  knowledge of Linear's webhook shape), `webhook.go` (HTTP handler),
  `nomad.go` (job dispatch).
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
  in a Go repo (including its own, `gastrodon/pibot`), and `psyduck` (from the
  `psyduck` flake input) + `bun` + a Firefox-only playwright browser set (from
  the `nixpkgs-playwright` flake input, pinned to match the npm `playwright`
  version `psyduck-etl/playwright-ts` embeds) so pibot can run
  `gastrodon/jobsearch-registry`'s `bin/check` when it's dispatched to work
  there. Nix store state isn't persisted across dispatches, and dotfiles' `free-code` and
  `ifunny-re` flake inputs are private repos fetched over `git+ssh` — pibot has
  no SSH key, so a build touching those inputs will fail to fetch them until
  that's resolved.
- **`module/pi-agent-system-prompt.md`** — the worker's operating manual, baked
  into the runtime image and passed as `--append-system-prompt`. Linear's
  workspace/team agent guidance is appended per-dispatch.

## Secrets

This repo owns **no secrets**. Every module option that needs one is a
`*File` path (`webhookSecretFile`, `clientIdFile`, `clientSecretFile`,
`nomadTokenFile`, `adminTokenFile`, `githubPatFile`,
`nomadBootstrapTokenFile`, `authFile`) — `config.go` reads `<KEY>_FILE` in preference to a
bare `<KEY>` env var. The consuming flake is responsible for decrypting
secret material and handing over paths, e.g. sops-nix's
`config.sops.secrets.<name>.path`.

Only *app-level* material is configured that way: the OAuth client
id/secret and the webhook signing secret, which belong to the one Linear app
and are the same for every workspace it serves. Per-workspace tokens are never
configured — they're minted by the install flow below and live in the
service's state directory.

## Installing into a workspace

The receiver starts with no Linear tokens at all, and is authorized after it's
deployed rather than before:

1. On the Linear OAuth app, register `${publicUrl}/oauth/callback` as a
   redirect URI and point the webhook at `${publicUrl}/webhook`.
2. Read the admin token off the box — the receiver mints one into its state
   directory on first start and reuses it across restarts:
   `ssh root@<host> cat /var/lib/linear-agent/admin-token`. (Pin it to your own
   secret material with `adminTokenFile` if you'd rather.)
3. Open `${publicUrl}/oauth/start?token=<that>` in a browser and approve. It
   redirects to Linear with `actor=app` and the agent scopes, catches the
   callback, exchanges the code, asks Linear which workspace authorized, and
   files the tokens under it in `/var/lib/linear-agent/tenants/<org-id>.json`.

Re-authorizing is step 3 again — no redeploy, no `sops set`, no restart.

Two gates sit on that flow, because a funnel makes both endpoints publicly
reachable. `/oauth/start` needs the admin token. `/oauth/callback` only honours
a `state` this process issued, once, within 10 minutes — and then refuses to
persist anything unless the workspace Linear names is in
`allowedOrganizations`. Leave that list empty only if you actually want any
workspace to be able to install this receiver.

`allowedOrganizations` is re-checked on every inbound event, not only at
install. The webhook signing secret belongs to the Linear *app*, so any
workspace that completes consent gets its events delivered here with a valid
signature — including one whose install was refused, since the code has to be
exchanged before Linear can be asked who consented. The event path is where
such a workspace is actually kept out.

The store is keyed per workspace, so one deployment can serve several: an
inbound event is routed by its `organizationId`, then by `appUserId`. A
workspace that is named but not held is refused outright — never routed to some
other install's credentials. The sole install is used as a fallback only for an
event that genuinely couldn't be matched: no `organizationId`, and either no
`appUserId` or an install whose app user id was never recorded. Run the install
flow against `publicUrl` itself — a second box behind the same configuration
issues states its peer won't recognize.

**Upgrading from the pre-install layout.** A receiver that predates this flow
kept one workspace's tokens in `/var/lib/linear-agent/token.json`, seeded from
a `refreshTokenFile`. That file is not migrated — it doesn't record which
workspace it belongs to — so the upgrade is: deploy, run the install flow, then
delete `token.json` (it still holds a refresh token). The receiver says as much
in its log when it finds one and has no installs.

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
            publicUrl = "https://server1.tailnet.ts.net";
            allowedOrganizations = [ "<linear workspace id>" ];
            webhookSecretFile = /* ... */;
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
