{
  # FlowHub — orchestration hub that turns DevOps events (YouTrack, later Gitea
  # and Drone) into work for a headless `opencode` instance.
  #
  # This flake provides a reproducible Go development environment
  # (`devShells.default`): Go toolchain, gopls, gofumpt, git, openssl. Activate it
  # with `direnv` (see .envrc) or `nix develop`.
  #
  # The canonical build/test workflow is driven by the Makefile
  # (`make build`, `make test`, `make lint`), which works unchanged inside the
  # dev shell.
  description = "FlowHub — DevOps/agent orchestration hub in Go";

  inputs = {
    # Pinned to the same known-good nixos-unstable snapshot as the sibling
    # TinyCode project. Its `go` attribute is still the 1.26 line, so the 1.27
    # toolchain is selected explicitly below.
    nixpkgs.url = "github:NixOS/nixpkgs/b1b875982b17dabde9b4a37f3e229e74913e6db3";
    flake-utils.url = "github:numtide/flake-utils/11707dc2f618dd54ca8739b309ec4fc024de578b";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system: let
      pkgs = import nixpkgs { inherit system; };
      # go_1_27 (not `go`) is deliberate: nixos-unstable still defaults to Go 1.26,
      # while go.mod and the Makefile target the modern toolchain. The `go 1.24`
      # directive in go.mod is a language-version floor, satisfied by 1.27.
      go = pkgs.go_1_27;
    in {
      devShells.default = pkgs.mkShell {
        nativeBuildInputs = with pkgs; [
          go
          git
          gopls
          gofumpt
          # `make secrets` uses openssl to generate the URL key and header token.
          openssl
          # `make smoke` drives a real process over HTTP and asserts the audit
          # trail with jq.
          curl
          jq
        ];

        shellHook = ''
          echo "[flowhub] nix dev shell  —  $(go version)"
          # Use the Nix-provided toolchain as-is (no auto-download from go.dev).
          export GOTOOLCHAIN=local
          # Drop any host GOROOT (e.g. /opt/go) so the Nix Go resolves its own
          # stdlib/tools — otherwise the compile tool versions mismatch.
          unset GOROOT
          # Shared, persistent module/build caches; respect a pre-set value.
          if [ -z "$GOMODCACHE" ]; then export GOMODCACHE="$HOME/.cache/go-mod"; fi
          if [ -z "$GOCACHE" ]; then export GOCACHE="$HOME/.cache/go-build"; fi
          # CGO_ENABLED is deliberately NOT forced to 0 here: `make test-race`
          # needs cgo for the race detector. Release and cross builds set
          # CGO_ENABLED=0 themselves in the Makefile.
          echo "[flowhub] CGO_ENABLED=''${CGO_ENABLED:-<platform default>}"
          echo "[flowhub] GOMODCACHE=$GOMODCACHE"
          echo "[flowhub] GOCACHE=$GOCACHE"
        '';
      };
    });
}
