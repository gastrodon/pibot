{
  description = "pibot — Linear agent-session webhook receiver + isolated pi worker";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";

    # The psyduck ETL host binary — the worker image bakes it in so pi can
    # drive real psyduck pipelines (e.g. jobsearch-registry's `bin/check`)
    # without fetching or building it per dispatch.
    psyduck = {
      url = "github:gastrodon/psyduck";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      psyduck,
      ...
    }:
    let
      systems = [ "x86_64-linux" ];
    in
    {
      packages = nixpkgs.lib.genAttrs systems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          linear-agent = import ./default.nix {
            inherit pkgs;
            inherit (pkgs) lib;
          };
          default = self.packages.${system}.linear-agent;
        }
      );

      formatter = nixpkgs.lib.genAttrs systems (system: nixpkgs.legacyPackages.${system}.nixfmt-tree);

      # linearAgent: the webhook receiver systemd service.
      # piAgent: the parameterized Nomad batch job + isolated pi worker it dispatches.
      # Both take secret material as file paths (options ending in *File) —
      # this repo owns no secrets; the consuming flake decrypts and hands over
      # paths (e.g. sops-nix's `config.sops.secrets.<name>.path`).
      nixosModules = rec {
        linearAgent = import ./module/linear-agent.nix;
        # piAgent is curried with the psyduck package for the caller's system
        # (resolved from `pkgs`, which NixOS always supplies) rather than
        # requiring the consuming flake to thread the input through itself.
        piAgent =
          args@{ pkgs, ... }:
          import ./module/pi-agent.nix (
            args // { psyduckPackage = psyduck.packages.${pkgs.system}.default; }
          );
        default = {
          imports = [
            linearAgent
            piAgent
          ];
        };
      };
    };
}
