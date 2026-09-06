# Linear agent-session webhook receiver. Verifies Linear's HMAC, acks the
# session with a `thought` activity, and dispatches a parameterized Nomad job
# (pi-agent) to run the actual work. The tunnel that fronts this is a separate
# concern — the receiver only binds loopback.
#
# There is no pre-minted Linear token here. A deployed receiver holds only the
# app-level credentials below and is authorized afterwards, by visiting
# `${publicUrl}/oauth/start?token=...` — which mints a per-workspace token into
# the service's StateDirectory. So `nixos-rebuild switch` never waits on an
# OAuth dance, and re-authorizing never needs a redeploy.
#
# This module owns no secrets: webhookSecretFile / clientIdFile /
# clientSecretFile / nomadTokenFile / adminTokenFile are paths to files holding
# the values (config.go reads <KEY>_FILE in preference to <KEY>). The consuming
# flake is responsible for decrypting and supplying those paths — e.g. via
# sops-nix's `config.sops.secrets.<name>.path`.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.linearAgent;
  linear-agent = import ../default.nix { inherit pkgs lib; };
in
{
  options.services.linearAgent = {
    enable = lib.mkEnableOption "Linear agent webhook receiver";

    listenAddr = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:3456";
      description = "Host:port the receiver binds. Loopback — a tunnel fronts it.";
    };

    nomadJob = lib.mkOption {
      type = lib.types.str;
      default = "pi-agent";
      description = "Parameterized Nomad batch job dispatched per session.";
    };

    webhookSecretFile = lib.mkOption {
      type = lib.types.path;
      description = "Path to a file containing the Linear webhook HMAC signing secret.";
    };

    publicUrl = lib.mkOption {
      type = lib.types.str;
      default = "";
      example = "https://server1.tailnet.ts.net";
      description = ''
        External base URL a tunnel fronts this receiver at, with no trailing
        slash. `''${publicUrl}/oauth/callback` is the OAuth redirect_uri, so it
        must be registered on the Linear app and must match byte-for-byte;
        `''${publicUrl}/webhook` is what Linear posts to. Empty disables the
        install flow — there is no redirect_uri to authorize against.

        Deliberately not derived from the inbound request's Host: a tunnel can
        rewrite that, and a redirect_uri that drifts fails the exchange.
      '';
    };

    adminTokenFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = ''
        Path to a file containing the token that gates `/oauth/start`, which is
        publicly reachable whenever a funnel fronts this port.

        Null (the default) is the deploy-first path: the receiver mints its own
        token into `/var/lib/linear-agent/admin-token` (0600) on first start
        and reuses it across restarts, so a box that has only ever been
        deployed can still be installed — read the file off the box to build
        the install URL. Set this only to pin the token to secret material you
        already manage.
      '';
    };

    allowedOrganizations = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "f9a4dcde-1f1d-43e1-a9c6-dbded1d624b4" ];
      description = ''
        Linear workspace (organization) ids allowed to complete an install. A
        consent that comes back for anything else is refused and its tokens are
        never persisted — checked against the workspace Linear names, not
        against anything the caller supplied, so it holds even if someone gets
        past the admin token.

        Empty (the default) accepts any workspace: that's the public
        multi-tenant posture, and the wrong one for a privately-hosted
        receiver.
      '';
    };

    clientIdFile = lib.mkOption {
      type = lib.types.path;
      description = "Path to a file containing the Linear OAuth app client id.";
    };

    clientSecretFile = lib.mkOption {
      type = lib.types.path;
      description = "Path to a file containing the Linear OAuth app client secret.";
    };

    nomadTokenFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Path to a file containing the Nomad ACL token used to dispatch jobs, if ACLs are enabled.";
    };

    defaultModel = lib.mkOption {
      type = lib.types.str;
      default = "anthropic/claude-sonnet-5";
      description = ''
        `provider/id` sent as the `model` dispatch Meta (pi's --model form)
        when a session carries no `pibot: model=...` override. Keep in sync
        with services.piAgent.provider + services.piAgent.model, which
        settings.json falls back to only when a dispatch carries no `model`
        Meta at all.
      '';
    };

    defaultThinking = lib.mkOption {
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
        Thinking level sent as the `thinking` dispatch Meta when a session
        carries no override. Keep in sync with services.piAgent.thinkingLevel.
      '';
    };

    allowedModels = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = ''
        Models a `pibot: model=...` directive may request. A directive naming
        anything else gets rejected with an `error` activity instead of being
        dispatched. Empty (the default) disables validation, so any directive
        is dispatched as-is; set this once there's a real roster of reachable
        models worth enforcing, e.g. `services.piAgent.defaultModel` plus an
        `ollama/<id>` once a local endpoint exists.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    users.users.linear-agent = {
      isSystemUser = true;
      group = "linear-agent";
      description = "Linear agent webhook receiver";
    };
    users.groups.linear-agent = { };

    systemd.services.linear-agent = {
      description = "Linear agent-session webhook receiver";
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      wantedBy = [ "multi-user.target" ];
      environment = {
        LISTEN_ADDR = cfg.listenAddr;
        NOMAD_ADDR = "http://127.0.0.1:4646";
        NOMAD_JOB = cfg.nomadJob;
        STATE_DIR = "/var/lib/linear-agent";
        LINEAR_WEBHOOK_SECRET_FILE = cfg.webhookSecretFile;
        LINEAR_CLIENT_ID_FILE = cfg.clientIdFile;
        LINEAR_CLIENT_SECRET_FILE = cfg.clientSecretFile;
        PUBLIC_URL = cfg.publicUrl;
        ALLOWED_ORGS = lib.concatStringsSep "," cfg.allowedOrganizations;
        DEFAULT_MODEL = cfg.defaultModel;
        DEFAULT_THINKING = cfg.defaultThinking;
        ALLOWED_MODELS = lib.concatStringsSep "," cfg.allowedModels;
      }
      // lib.optionalAttrs (cfg.nomadTokenFile != null) {
        NOMAD_TOKEN_FILE = cfg.nomadTokenFile;
      }
      // lib.optionalAttrs (cfg.adminTokenFile != null) {
        ADMIN_TOKEN_FILE = cfg.adminTokenFile;
      };
      serviceConfig = {
        ExecStart = "${linear-agent}/bin/linear-agent";
        User = "linear-agent";
        Group = "linear-agent";
        Restart = "always";
        RestartSec = 5;
        # /var/lib/linear-agent, owned by the service user — holds the
        # per-workspace OAuth tokens minted by the install flow (and rotated
        # in place) plus the self-minted admin token; stays writable under
        # ProtectSystem=strict. Losing it means re-running the install flow.
        StateDirectory = "linear-agent";
        StateDirectoryMode = "0700";
        # Hardening — network service; only writes are to StateDirectory.
        DynamicUser = false;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        NoNewPrivileges = true;
      };
    };
  };
}
