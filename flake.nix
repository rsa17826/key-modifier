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
            vendorHash = "sha256-eZe3PyvnuU4h0tM1pdRy5LKceBAY83QXnN1ZuSGzerE=";

            nativeBuildInputs = [ pkgs.installShellFiles ];

            postInstall = ''
              installShellCompletion --zsh --name _key-modifier completions/_key-modifier
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
