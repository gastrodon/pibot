# pi-agent — the per-session worker the linear-agent receiver dispatches. This
# is a parameterized Nomad batch job: the receiver POSTs the raw webhook as the
# dispatch payload and passes session_id + a short-lived Linear access token in
# Meta. The task runs the pi coding agent (pi-black routing Anthropic through the
# subscription) headlessly and posts its output back as a Linear `response`.
#
# This module owns no secrets: githubPatFile / nomadBootstrapTokenFile are paths
# to files holding the values. The consuming flake is responsible for
# decrypting and supplying those paths — e.g. via sops-nix's
# `config.sops.secrets.<name>.path`.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.piAgent;

  jsonFormat = pkgs.formats.json { };

  # settings.json for the pi agent. Nix is authoritative — the entrypoint copies
  # this over the volume copy every dispatch, so Eva's tweaks flow through
  # rebuilds. defaultProjectTrust=always is required because -p is non-interactive
  # (no trust prompt) and pi-black is loaded as a package.
  #
  # These defaults are what every dispatch actually runs: the receiver
  # (linear-agent's dispatchNomad) sends no `model` Meta, so the entrypoint's
  # --model/--thinking overrides are unset for Linear-originated sessions and pi
  # falls back to settings.json. Routing the fleet at the ollama provider below
  # is therefore a settings change, not a receiver change.
  settingsFile = jsonFormat.generate "pi-settings.json" {
    defaultProvider = cfg.provider;
    defaultModel = cfg.model;
    defaultThinkingLevel = cfg.thinkingLevel;
    defaultProjectTrust = "always";
    packages = [ "git:github.com/paoloanzn/pi-black@v0.84.1-cc2.1.224.4" ];
  };

  # Opt-in models.json for a local Ollama provider, alongside settingsFile. Only
  # generated when services.piAgent.ollama.enable is set, so the default
  # (Anthropic-only, via pi-black) is unaffected. Shape matches what pi expects
  # for an OpenAI-completions-compatible provider: compat flags turn off
  # features Ollama's endpoint doesn't support. pi's models.json schema wants
  # `models` as a list of objects ({id: ...}), not bare strings — verified
  # against the real pi 0.84.1 binary (`pi --list-models`) before landing this.
  modelsFile = jsonFormat.generate "pi-models.json" {
    providers = {
      ollama = {
        baseUrl = cfg.ollama.baseUrl;
        api = "openai-completions";
        apiKey = "ollama";
        compat = {
          supportsDeveloperRole = false;
          supportsReasoningEffort = false;
        };
        models = [ { id = cfg.ollama.model; } ];
      };
    };
  };

  # Unpatched standalone pi. It's a Bun single-exec (glibc-dynamic) — we do NOT
  # autoPatchelf it (patchelf-on-appended-payload breaks Bun single-execs). The
  # interp is the FHS path /lib64/ld-linux-x86-64.so.2; piImage carries glibc,
  # which supplies that loader, so the binary runs unmodified. Keep the tarball
  # layout intact (sibling
  # node_modules + wasm). settings.json rides along so the entrypoint can cp it
  # from the ro mount.
  piPkg = pkgs.stdenvNoCC.mkDerivation {
    pname = "pi-standalone";
    version = "0.84.1";
    src = pkgs.fetchurl {
      url = "https://github.com/earendil-works/pi/releases/download/v0.84.1/pi-linux-x64.tar.gz";
      sha256 = "sha256-VjTX69GCdLY68zcelC80LXS+oBI4lXXB0f8VzmyoDC8=";
    };
    dontPatchELF = true;
    dontStrip = true;
    installPhase = ''
      mkdir -p $out
      cp -r . $out/
      cp ${settingsFile} $out/settings.json
      ${lib.optionalString cfg.ollama.enable ''
        cp ${modelsFile} $out/models.json
      ''}
      cp ${entrypointScript} $out/entrypoint.sh
      cp ${./pi-agent-system-prompt.md} $out/system-prompt.md
    '';
  };

  # From-scratch runtime image, nix-built. It reaches both server boxes through
  # the system closure: the job JSON embeds "docker-archive:${piImage}" and the
  # podman driver's docker-archive: transport ImageLoads that store path — no
  # registry, no push. Carries only the entrypoint's needs: git (clone/push +
  # pi-black's git: install), nodejs (pi-black's install shells `npm install`),
  # curl (Linear post), jq (JSON build), gh (PR creation, reads GH_TOKEN), grep +
  # findutils (pi's bash tool reflexively shells `find … | grep …` to explore —
  # coreutils supplies neither), bash, coreutils, cacert. nix lets pibot
  # build/test gastrodon/dotfiles the same way CI does — `nix build
  # .#nixosConfigurations.<host>.config.system.build.toplevel --impure` and
  # `nix flake check`/`nixfmt --check`. sandbox is disabled: the podman task runs
  # unprivileged and can't create the user/mount namespaces a sandboxed nix build
  # needs; build-users-group is left unset so nix builds directly as the container's
  # root user (no nixbld users exist here). Store state is not persisted across
  # dispatches — see README for why, and the still-open gap around private
  # git+ssh flake inputs (e.g. free-code) whose fetch requires an SSH key.
  # pi itself is NOT baked — it rides the /opt/pi bind-mount. glibc supplies
  # /lib64/ld-linux-x86-64.so.2 so the unpatched Bun exec runs here.
  piImage = pkgs.dockerTools.buildLayeredImage {
    name = "pibot-pi";
    tag = "latest";
    contents = [
      pkgs.git
      pkgs.nodejs
      pkgs.curl
      pkgs.jq
      pkgs.gh
      pkgs.gnugrep
      pkgs.findutils
      pkgs.bashInteractive
      pkgs.coreutils
      pkgs.cacert
      pkgs.glibc
      pkgs.nix
    ];
    extraCommands = ''
      mkdir -p tmp var/tmp
    '';
    config = {
      Env = [
        "PATH=/bin"
        "LD_LIBRARY_PATH=${pkgs.glibc}/lib"
        "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
        "GIT_SSL_CAINFO=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
        "NIX_SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
        "NIX_CONFIG=experimental-features = nix-command flakes\nsandbox = false"
        "SHELL=/bin/bash"
        "HOME=/root"
      ];
      WorkingDir = "/";
    };
  };

  # /opt/pi is the ro nix-store mount (piPkg); /root/.pi/agent is the rw
  # persistent volume (auth.json + pi-black install + trust survive dispatches).
  # Meta lands as NOMAD_META_<key>.
  entrypoint = ''
    set -eu
    export HOME=/root
    mkdir -p "$HOME/.pi/agent"
    cp -f /opt/pi/settings.json "$HOME/.pi/agent/settings.json"
    if [ -f /opt/pi/models.json ]; then
      cp -f /opt/pi/models.json "$HOME/.pi/agent/models.json"
    else
      # models.json rides a persistent volume (/root/.pi/agent survives
      # dispatches); if ollama is disabled after previously being enabled, a
      # stale copy would keep pointing pi at a dead endpoint forever. Force
      # this to match settings.json's unconditional overwrite above.
      rm -f "$HOME/.pi/agent/models.json"
    fi

    # GitHub auth for clone/push. The PAT rides in on a ro bind-mount of the sops
    # secret; feed it to git via a credential helper (keeps it out of .gitconfig)
    # and export GH_TOKEN/GITHUB_TOKEN — gh (baked in the image) reads these for
    # PR creation, no separate `gh auth login` needed.
    if [ -f /run/github-pat ]; then
      GH_TOKEN="$(cat /run/github-pat)"
      export GH_TOKEN
      export GITHUB_TOKEN="$GH_TOKEN"
      git config --global credential.helper '!f() { echo username=x-access-token; echo "password=$GH_TOKEN"; }; f'
      git config --global user.name pibot
      git config --global user.email pibot@users.noreply.github.com
    fi

    # Linear access for pi, as the pibot APP identity (never Eva's personal key).
    # The receiver passes a short-lived OAuth app token per dispatch in
    # NOMAD_META_access_token; surface it as LINEAR_ACCESS_TOKEN so pi's run can
    # reach it. It's an OAuth token, so Linear wants `Authorization: Bearer
    # <token>` (curl+jq — schpet/linear-cli can't carry it: it sends the key raw
    # with no Bearer prefix, which only authenticates personal API keys).
    if [ -n "''${NOMAD_META_access_token:-}" ]; then
      export LINEAR_ACCESS_TOKEN="$NOMAD_META_access_token"
    fi

    # Extract the prompt from the raw webhook. The exact field is unconfirmed
    # (Go passes the payload through unparsed) — try promptContext, then the
    # nested form, then fall back to the whole payload so we never send empty.
    # jq -r prints a string bare and an object as compact JSON; .a.b is null-safe.
    prompt=$(jq -r '.promptContext // .agentSession.promptContext // tojson' /local/webhook.json)

    model_args=""
    if [ -n "''${NOMAD_META_model:-}" ]; then model_args="--model ''${NOMAD_META_model}"; fi
    think_args=""
    if [ -n "''${NOMAD_META_thinking:-}" ]; then think_args="--thinking ''${NOMAD_META_thinking}"; fi

    # System prompt: the Nix-managed operating manual (edited as prose in
    # pi-agent-system-prompt.md), with Linear's workspace/team agent guidance
    # appended per-dispatch. Linear delivers guidance as a top-level webhook field
    # (free-form markdown for the agent); empty when unset.
    sys=$(cat /opt/pi/system-prompt.md)
    guidance=$(jq -r '.guidance // ""' /local/webhook.json)
    if [ -n "$guidance" ]; then
      sys="$sys

## Workspace agent guidance (from Linear)
$guidance"
    fi

    # Drive pi in RPC mode. -p and --mode json do NOT autonomously continue the
    # agent loop headlessly on this build (upstream pi 0.84.1 stdin bug: after a
    # tool result the run aborts at turn 2 with no agent_end — earendil-works/pi
    # #4303/#2381). RPC holds the connection open and runs the full multi-turn loop
    # to agent_settled. We open a fifo as pi's stdin, send one prompt, stream events
    # to pi.jsonl, wait for agent_settled, then close stdin so pi exits cleanly.
    # ask_question is disabled (a no-op headlessly); the system prompt tells pi to
    # ask by ending its turn with a question instead.
    fifo=/local/rpcin
    rm -f "$fifo"
    mkfifo "$fifo"
    /opt/pi/pi --mode rpc \
      --append-system-prompt "$sys" \
      --exclude-tools ask_question \
      $model_args $think_args <"$fifo" >/local/pi.jsonl 2>/local/pi-err.txt &
    pipid=$!
    exec 3>"$fifo"
    printf '{"type":"prompt","message":%s}\n' "$(printf '%s' "$prompt" | jq -Rs .)" >&3

    # Wait for the session to settle, bounded by a 30m deadline.
    #
    # The sentinel MUST be agent_settled, not agent_end. agent_end fires once per
    # *low-level* agent run and is explicitly "may still be followed by retry,
    # compaction, or queued continuations" (docs/rpc.md); it carries willRetry:true
    # when an auto-retry (529/rate-limit/5xx) or an overflow-compaction retry is
    # about to resume the run. Breaking on the first agent_end therefore killed pi
    # mid-flight on any retried session: the work was left half-done, and because
    # the turns up to that point are typically thinking+toolCall with no text block,
    # the reply came out empty and Linear got the "pi completed without a text
    # response." fallback. agent_settled is the terminal event — no retry,
    # compaction retry, or queued continuation remains. (Added upstream in 0.80.4;
    # this image pins 0.84.1, so it is always emitted.)
    deadline=$(( $(date +%s) + 1800 ))
    while kill -0 "$pipid" 2>/dev/null; do
      if grep -q '"type":"agent_settled"' /local/pi.jsonl 2>/dev/null; then break; fi
      if [ "$(date +%s)" -ge "$deadline" ]; then break; fi
      sleep 2
    done

    # Close stdin so pi exits after the run settles; force-kill if it lingers.
    exec 3>&-
    for _ in $(seq 1 10); do kill -0 "$pipid" 2>/dev/null || break; sleep 1; done
    kill "$pipid" 2>/dev/null || true
    if wait "$pipid" 2>/dev/null; then rc=0; else rc=$?; fi
    # agent_settled means the work completed even if we had to close/kill to exit.
    if grep -q '"type":"agent_settled"' /local/pi.jsonl 2>/dev/null; then rc=0; fi

    # Assemble the reply from the authoritative message_end events — clean assistant
    # text blocks only. (Do NOT concatenate message_update deltas: that stream
    # interleaves thinking text and raw tool-call arg JSON.)
    #
    # Take the LAST assistant message that has text, not every one concatenated:
    # the system prompt tells pi its *final* message is what gets posted, so the
    # intermediate between-tool narration is not meant for the Linear thread.
    # Emit as a compact JSON string (one line, newlines escaped) so `tail -n 1`
    # picks the final record even when the reply is multi-line, then decode it.
    # Streaming jq (not -s) also means a truncated final line — possible when we
    # had to kill pi — is skipped instead of voiding the whole extraction.
    jq -c 'select(.type=="message_end") | .message | select(.role=="assistant")
           | [.content[]? | select(.type=="text") | .text] | join("\n")
           | select(length > 0)' /local/pi.jsonl 2>/dev/null \
      | tail -n 1 > /local/pi-last.json || true
    jq -r '.' /local/pi-last.json > /local/pi-out.txt 2>/dev/null || : > /local/pi-out.txt

    if [ "$rc" -ne 0 ]; then
      act=error
      { echo "pi exited nonzero (rc=$rc):"; tail -n 40 /local/pi-err.txt; } > /local/pi-out.txt
    elif [ ! -s /local/pi-out.txt ]; then
      # Never post an empty comment: fall back to a tool-run summary so the Linear
      # thread always carries signal about what pi did.
      act=response
      { echo "pi completed without a text response.";
        tools=$(jq -r 'select(.type=="tool_execution_start") | .toolName' /local/pi.jsonl 2>/dev/null | sort | uniq -c);
        if [ -n "$tools" ]; then echo; echo "tools run:"; echo "$tools"; fi
      } > /local/pi-out.txt
    else
      act=response
    fi

    # Build the GraphQL body with jq so pi's output (quotes/newlines/backslashes)
    # is correctly JSON-escaped — shell string-building would produce invalid JSON.
    # --rawfile reads pi-out.txt as a string var, so the body is escaped safely.
    jq -n \
      --rawfile body /local/pi-out.txt \
      --arg session "$NOMAD_META_session_id" \
      --arg act "$act" \
      '{query:"mutation($input: AgentActivityCreateInput!) { agentActivityCreate(input: $input) { success } }",variables:{input:{agentSessionId:$session,content:{type:$act,body:$body}}}}' \
      > /local/req.json
    curl -sS -X POST https://api.linear.app/graphql \
      -H "Content-Type: application/json" \
      -H "Authorization: Bearer $NOMAD_META_access_token" \
      --data @/local/req.json
  '';

  # Ship the script as a file in piPkg and run it, rather than inlining it as the
  # task's args: Nomad interpolates ''${...} in job-spec fields and rejects the
  # bash ''${VAR:-} colon syntax. In a file bash owns all expansion; Nomad only
  # injects NOMAD_META_* as env.
  entrypointScript = pkgs.writeText "pi-agent-entrypoint.sh" entrypoint;

  # API JSON shape for POST /v1/jobs — the { Job = {...}; } wrapper is the exact
  # request body. Meta keys declared here must match dispatchNomad exactly or
  # Nomad rejects the dispatch. model/thinking are optional (receiver doesn't send
  # them yet — defaults come from settings.json; per-request routing is future work).
  jobFile = jsonFormat.generate "pi-agent.json" {
    Job = {
      ID = "pi-agent";
      Name = "pi-agent";
      Type = "batch";
      Datacenters = [ "home" ];
      ParameterizedJob = {
        Payload = "optional";
        MetaRequired = [ "session_id" ];
        MetaOptional = [
          "action"
          "access_token"
          "model"
          "thinking"
        ];
      };
      TaskGroups = [
        {
          Name = "pi";
          Count = 1;
          # Pin to the server boxes — only they carry piPkg's store path and the
          # /var/lib/pi-agent/home auth volume (rpi clients are arm64 and lack both).
          Constraints = [
            {
              LTarget = "\${meta.pi_worker}";
              RTarget = "true";
              Operand = "=";
            }
          ];
          Tasks = [
            {
              Name = "pi";
              Driver = "podman";
              Config = {
                image = "docker-archive:${piImage}";
                command = "bash";
                args = [ "/opt/pi/entrypoint.sh" ];
                volumes = [
                  "${piPkg}:/opt/pi:ro"
                  "/var/lib/pi-agent/home:/root/.pi/agent"
                  "${cfg.githubPatFile}:/run/github-pat:ro"
                ];
              };
              Resources = {
                CPU = 1000;
                MemoryMB = 1024;
              };
              DispatchPayload = {
                File = "webhook.json";
              };
            }
          ];
        }
      ];
    };
  };
in
{
  options.services.piAgent = {
    enable = lib.mkEnableOption "pi-agent Nomad job (the isolated pi worker linear-agent dispatches)";

    githubPatFile = lib.mkOption {
      type = lib.types.path;
      description = "Path to a file containing a GitHub PAT for the worker's clone/push + PR creation.";
    };

    nomadBootstrapTokenFile = lib.mkOption {
      type = lib.types.path;
      description = "Path to a file containing the Nomad ACL management token, used to register the job.";
    };

    provider = lib.mkOption {
      type = lib.types.str;
      default = "anthropic";
      description = ''
        Provider every dispatched session routes to, written to settings.json as
        defaultProvider. "anthropic" (via pi-black, on the subscription) by
        default; set to "ollama" — together with ollama.enable — to route the
        whole fleet at the local endpoint. This is fleet-wide; per-session
        routing needs the receiver to send a `model` dispatch Meta, which it
        does not do yet.
      '';
    };

    model = lib.mkOption {
      type = lib.types.str;
      default = "claude-sonnet-5";
      description = ''
        Model id every dispatched session runs, written to settings.json as
        defaultModel. Must be a model the configured provider exposes: a catalog
        id for a built-in provider (verify with `pi update --models`), or
        ollama.model when provider = "ollama".
      '';
    };

    thinkingLevel = lib.mkOption {
      type = lib.types.enum [
        "off"
        "minimal"
        "low"
        "medium"
        "high"
        "xhigh"
        "max"
      ];
      default = "high";
      description = ''
        Thinking level written to settings.json as defaultThinkingLevel. Ollama
        generally wants "off" — most local models don't support reasoning, and
        compat.supportsReasoningEffort is disabled for the ollama provider.
      '';
    };

    ollama = {
      enable = lib.mkEnableOption "a local Ollama provider stanza in ~/.pi/agent/models.json, for --model ollama/<id> dispatches";

      baseUrl = lib.mkOption {
        type = lib.types.str;
        default = "http://ollama:11434/v1";
        description = "OpenAI-completions-compatible base URL of the Ollama endpoint, reachable from the container network.";
      };

      model = lib.mkOption {
        type = lib.types.str;
        description = "The single model id pulled on the Ollama endpoint, exposed as ollama/<id>. An ollama-routed worker runs exactly one model; there's no meaningful case for a list here.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    # Routing at ollama without generating models.json leaves pi with a provider
    # it has never heard of: the model fails to resolve and every dispatch dies
    # with `Model "ollama/<id>" not found`. Catch it at eval instead.
    assertions = [
      {
        assertion = cfg.provider == "ollama" -> cfg.ollama.enable;
        message = ''
          services.piAgent.provider = "ollama" requires services.piAgent.ollama.enable = true,
          otherwise no models.json is generated and the model cannot resolve.
        '';
      }
      {
        assertion = (cfg.provider == "ollama" && cfg.ollama.enable) -> (cfg.model == cfg.ollama.model);
        message = ''
          services.piAgent.model ("${cfg.model}") must match services.piAgent.ollama.model
          ("${cfg.ollama.model}") when routing at ollama — models.json declares exactly one
          model, and settings.json must name that one.
        '';
      }
    ];

    # Persistent rw container home: auth.json, pi-black install, trust,
    # sessions. Survives dispatches; the login command seeds auth.json here once.
    systemd.tmpfiles.rules = [
      "d /var/lib/pi-agent/home 0700 root root -"
    ];

    # Register (idempotent upsert) the job once Nomad has a leader. Cloned from
    # nomad-acl-bootstrap: same retry-until-leader loop, same token handling. Both
    # server boxes run this; a re-register is a no-op.
    systemd.services.pi-agent-register = {
      description = "Register the pi-agent parameterized Nomad batch job";
      after = [ "nomad-acl-bootstrap.service" ];
      requires = [ "nomad-acl-bootstrap.service" ];
      wantedBy = [ "multi-user.target" ];
      path = [ pkgs.curl ];
      environment.NOMAD_ADDR = "http://127.0.0.1:4646";
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
      };
      script = ''
        set -u
        umask 077
        tmp=$(mktemp)
        trap 'rm -f "$tmp"' EXIT
        tr -d '[:space:]' < ${cfg.nomadBootstrapTokenFile} > "$tmp"
        token=$(cat "$tmp")

        for _ in $(seq 1 60); do
          code=$(curl -s -o /dev/null -w '%{http_code}' \
            -H "X-Nomad-Token: $token" \
            -X POST "$NOMAD_ADDR/v1/jobs" \
            --data @${jobFile}) || code=000
          case "$code" in
            200)
              echo "pi-agent job registered"
              exit 0
              ;;
            *)
              sleep 2
              ;;
          esac
        done
        echo "pi-agent registration failed after retries" >&2
        exit 1
      '';
    };
  };
}
