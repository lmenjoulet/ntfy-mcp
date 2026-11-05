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
      "-s"
      "-w"
    ];
    env.CGO_ENABLED = "0";
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
      # scripts/run-tests.sh parses MCP responses with jq; CI images
      # (GitHub Actions) ship it preinstalled, the dev shell provides it.
      jq
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

      address = lib.mkOption {
        type = lib.types.str;
        default = "127.0.0.1";
        description = "Address the MCP server listens on.";
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

      clientTokenFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        description = ''
    Fichier contenant le secret partagé exigé des clients MCP, au format
    MCP_NTFY_CLIENT_TOKEN=... (une ligne). Quand il est renseigné, toute
    requête MCP doit porter ce secret en en-tête
    "Authorization: Bearer ..." ; utile pour empêcher les utilisateurs
    non authentifiés de spammer les notifications quand le serveur est
    exposé au-delà de localhost. Préféré à un secret en clair dans la
    configuration.
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
      "MCP_NTFY_ADDR=${toString cfg.address}"
      "MCP_NTFY_PORT=${toString cfg.port}"
      "NTFY_URL=${cfg.ntfyUrl}"
      "NTFY_TOPIC=${cfg.ntfyTopic}"
    ];
    EnvironmentFile =
      (lib.optional (cfg.tokenFile != null) cfg.tokenFile)
      ++ (lib.optional (cfg.clientTokenFile != null) cfg.clientTokenFile);
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
