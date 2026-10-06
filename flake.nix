{
  description = "pbctl: a PocketBase CLI and MCP server for agents, with an enforceable read-only mode";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forEachSystem = build: nixpkgs.lib.genAttrs systems (system: build nixpkgs.legacyPackages.${system});
      version = self.shortRev or self.dirtyShortRev or "dev";
      pbctlFor =
        pkgs:
        pkgs.buildGoModule {
          pname = "pbctl";
          inherit version;
          src = self;
          vendorHash = "sha256-nsnIqihq0ITh3uhFZI0/kBXKWHEN3fJu96BKvpu68Y4=";
          subPackages = [ "cmd/pbctl" ];
          env.CGO_ENABLED = 0;
          ldflags = [
            "-s"
            "-w"
            "-X main.version=${version}"
          ];
          meta = {
            description = "PocketBase CLI and MCP server for agents, with an enforceable read-only mode";
            homepage = "https://github.com/Barney241/pocketbase-cli";
            license = nixpkgs.lib.licenses.mit;
            mainProgram = "pbctl";
          };
        };
    in
    {
      packages = forEachSystem (pkgs: rec {
        pbctl = pbctlFor pkgs;
        default = pbctl;
      });

      apps = forEachSystem (pkgs: rec {
        pbctl = {
          type = "app";
          program = nixpkgs.lib.getExe (pbctlFor pkgs);
          meta.description = "Run pbctl";
        };
        default = pbctl;
      });

      overlays.default = final: _: { pbctl = pbctlFor final; };

      devShells = forEachSystem (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gnumake
            pkgs.goreleaser
          ];
        };
      });
    };
}
