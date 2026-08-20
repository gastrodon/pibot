{
  description = "pibot — Linear agent-session webhook receiver + isolated pi worker";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";

    # The psyduck host CLI, so the pi worker's image can run a psyduck pipeline
    # directly (e.g. jobsearch-registry's `bin/check`, which drives
    # `psyduck init`/`psyduck run`). Its flake exposes packages.default.
    psyduck = {
      url = "github:gastrodon/psyduck";
      inputs.nixpkgs.follows = "nixpkgs";
    };

    # nixpkgs pinned to playwright-driver 1.61.1, matching the npm
    # `playwright` version psyduck-etl/playwright-ts pins in package.json —
    # the nix-provided browser revision has to match the driver version a
    # plugin build embeds. That's well ahead of nixos-25.11's own bundled
    # playwright-driver (1.56.1), hence the separate pin; jobsearch-etl
    # documents the same revision-matching constraint for its own (older,
    # Go-driven) playwright pin.
    nixpkgs-playwright.url = "github:NixOS/nixpkgs/f2676046a1cba86d6f86a64f5b6d427b2f4dec96";
  };

  outputs =
    {
      self,
      nixpkgs,
      psyduck,
      nixpkgs-playwright,
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
        # Wraps the module to hand it the psyduck binary and a Firefox-only
        # playwright browser set built from the pinned inputs above — pulled in
        # here (not inside module/pi-agent.nix) because a plain nixosModule has
        # no access to this flake's own inputs otherwise.
        piAgent =
          {
            config,
            lib,
            pkgs,
            ...
          }@args:
          import ./module/pi-agent.nix (
            args
            // {
              psyduckPkg = psyduck.packages.${pkgs.system}.default;
              playwrightBrowsers =
                nixpkgs-playwright.legacyPackages.${pkgs.system}.playwright-driver.browsers.override
                  {
                    withChromium = false;
                    withChromiumHeadlessShell = false;
                    withWebkit = false;
                    withFfmpeg = false;
                  };
            }
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
