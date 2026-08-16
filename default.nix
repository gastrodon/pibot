{ pkgs, lib, ... }:

pkgs.buildGoModule {
  pname = "linear-agent";
  version = "0.1.0";

  src = ./.;

  # stdlib-only, no external modules
  vendorHash = null;

  # `go install` names the binary after the last element of the module path
  # (github.com/gastrodon/pibot → `pibot`), but the service this ships — unit,
  # user, StateDirectory — is `linear-agent`, and module/linear-agent.nix execs
  # ${placeholder "out"}/bin/linear-agent. Rename so the two agree.
  postInstall = ''
    mv $out/bin/pibot $out/bin/linear-agent
  '';

  meta = with lib; {
    description = "Linear agent-session webhook receiver → Nomad dispatch";
    license = licenses.mit;
    mainProgram = "linear-agent";
    maintainers = [ ];
  };
}
