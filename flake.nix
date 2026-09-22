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
            pname = "key-modifier";
            version = "2";
            src = ./.;
            vendorHash = "sha256-xhuMZsi0g5OWKA0BnqBQT+OhThfM5/ZDJKn5Muwqbsg=";

            nativeBuildInputs = [ pkgs.installShellFiles ];

            postInstall = ''
              installShellCompletion --zsh --name _keymod completions/_keymod
            '';
          };
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
