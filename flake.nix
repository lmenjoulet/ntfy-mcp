{
  description = "ntfy MCP server";
  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      supportedSystems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];

      forEachSystem = f: nixpkgs.lib.genAttrs supportedSystems (system: f {
  pkgs = import nixpkgs { inherit system; };
      });
    in
    {
      packages = forEachSystem ({ pkgs }: {
  ntfy-mcp = pkgs.buildGoModule rec {
    pname = "ntfy-mcp";
    version = "0.1.0";
    src = self;
    ldflags = [
      "-X main.Version=${version}"
    ];
    vendorHash = "sha256-MUVW6/ZiQ5h8AUVbu/yzVhUEGvA5NoxdAYH50skkak4=";
    meta = with pkgs.lib; {
      description = "Serveur MCP d'envoi de notifications ntfy";
      mainProgram = "ntfy-mcp";
      platforms = supportedSystems;
    };
  };
  default = self.packages.${pkgs.stdenv.hostPlatform.system}.ntfy-mcp;
      });

      devShells = forEachSystem ({ pkgs }: {
  default = pkgs.mkShell {
    packages = with pkgs; [
      go
    ];

    shellHook = ''
      export GOPATH="$HOME/go"
      export PATH="$GOPATH/bin:$PATH"

      export PS1="$PS1(Go) "
    '';
  };
      });

      nixosModules.default = { config, pkgs, lib, ... }:
  let
    cfg = config.services.ntfy-mcp;
  in
  {
    options.services.ntfy-mcp = {
      enable = lib.mkEnableOption "le serveur MCP ntfy";

      package = lib.mkOption {
        type = lib.types.package;
        default = self.packages.${pkgs.stdenv.hostPlatform.system}.ntfy-mcp;
        description = "Paquet ntfy-mcp à utiliser.";
      };

      port = lib.mkOption {
        type = lib.types.port;
        default = 12120;
        description = "Port d'écoute du serveur MCP (chemin /mcp).";
      };

      ntfyUrl = lib.mkOption {
        type = lib.types.str;
        default = "https://ntfy.sh";
        description = "URL du serveur ntfy par défaut.";
      };

      ntfyTopic = lib.mkOption {
        type = lib.types.str;
        default = "agent";
        description = "Topic ntfy par défaut.";
      };

      tokenFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        description = ''
    Fichier contenant le jeton d'accès ntfy, au format
    NTFY_TOKEN=... (une ligne). Préféré à un jeton en clair
    dans la configuration.
        '';
      };
    };

    config = lib.mkIf cfg.enable {
      systemd.services.ntfy-mcp = {
        description = "Serveur MCP d'envoi de notifications ntfy";
        after = [ "network-online.target" ];
        wants = [ "network-online.target" ];
        wantedBy = [ "multi-user.target" ];

        serviceConfig = {
    ExecStart = lib.getExe cfg.package;
    Environment = [
      "MCP_NTFY_PORT=${toString cfg.port}"
      "NTFY_URL=${cfg.ntfyUrl}"
      "NTFY_TOPIC=${cfg.ntfyTopic}"
    ];
    EnvironmentFile = lib.mkIf (cfg.tokenFile != null) [ cfg.tokenFile ];
    DynamicUser = true;
    NoNewPrivileges = true;
    PrivateTmp = true;
    ProtectSystem = "strict";
    ProtectHome = true;
    Restart = "on-failure";
        };
      };
    };
  };

      overlays.default = final: prev: {
  ntfy-mcp = self.packages.${final.system}.ntfy-mcp;
      };
    };
}
