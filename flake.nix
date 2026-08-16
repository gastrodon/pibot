{
  description = "pibot — Linear agent-session webhook receiver + isolated pi worker";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";
  };

  outputs =
    { self, nixpkgs, ... }:
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
        piAgent = import ./module/pi-agent.nix;
        default = {
          imports = [
            linearAgent
            piAgent
          ];
        };
      };
    };
}
