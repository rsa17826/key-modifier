{
  inputs = {
    nixpkgs = {
      url = "github:NixOS/nixpkgs/nixos-unstable";
    };
    flake-utils = {
      url = "github:numtide/flake-utils";
    };
  };

  outputs =
    {
      nixpkgs,
      flake-utils,
      ...
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs { inherit system; };
      in
      {
        packages = {
          # The actual package
          default = pkgs.buildGoModule {
            pname = "input-manager";
            version = "2";
            src = ./.;
            vendorHash = "sha256-M4L7SYuisYPNC3AqMwFLs3fZGVmgmdZ9ZmZih4IgpvM=";
          };
          nativeBuildInputs = [ pkgs.installShellFiles ];

          postInstall = ''
            installShellCompletion --zsh --name _keymod completions/_keymod
          '';
        };
        devShells = {
          # Development environment
          default = pkgs.mkShell {
            buildInputs = with pkgs; [
              go
              gopls
            ];
          };
        };
      }
    );
}
