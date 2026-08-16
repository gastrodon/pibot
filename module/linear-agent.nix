# Linear agent-session webhook receiver. Verifies Linear's HMAC, acks the
# session with a `thought` activity, and dispatches a parameterized Nomad job
# (pi-agent) to run the actual work. The tunnel that fronts this is a separate
# concern — the receiver only binds loopback.
#
# This module owns no secrets: webhookSecretFile / refreshTokenFile /
# clientIdFile / clientSecretFile / nomadTokenFile are paths to files holding
# the values (main.go reads <KEY>_FILE in preference to <KEY>). The consuming
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

    refreshTokenFile = lib.mkOption {
      type = lib.types.path;
      description = "Path to a file containing the Linear OAuth refresh token.";
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
        `provider/id` sent as the `model` dispatch Meta on every session
        (pi's --model form), until per-request routing (EVA-111) can pick a
        different value per dispatch. Keep in sync with
        services.piAgent.provider + services.piAgent.model, which settings.json
        falls back to only when a dispatch carries no `model` Meta at all.
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
        Thinking level sent as the `thinking` dispatch Meta on every session.
        Keep in sync with services.piAgent.thinkingLevel.
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
        LINEAR_REFRESH_TOKEN_FILE = cfg.refreshTokenFile;
        LINEAR_CLIENT_ID_FILE = cfg.clientIdFile;
        LINEAR_CLIENT_SECRET_FILE = cfg.clientSecretFile;
        DEFAULT_MODEL = cfg.defaultModel;
        DEFAULT_THINKING = cfg.defaultThinking;
      }
      // lib.optionalAttrs (cfg.nomadTokenFile != null) {
        NOMAD_TOKEN_FILE = cfg.nomadTokenFile;
      };
      serviceConfig = {
        ExecStart = "${linear-agent}/bin/linear-agent";
        User = "linear-agent";
        Group = "linear-agent";
        Restart = "always";
        RestartSec = 5;
        # /var/lib/linear-agent, owned by the service user — holds the rotated
        # OAuth token state; stays writable under ProtectSystem=strict.
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
