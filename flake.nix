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
    version = "1.1.1";
    src = self;
    ldflags = [
      "-X main.Version=${version}"
      "-s"
      "-w"
    ];
    env.CGO_ENABLED = "0";
    vendorHash = "sha256-xfaB0gV5j1bPu1du6fzKq5IIiubWqaYez9n1UMcStHw=";
    meta = with pkgs.lib; {
      description = "Serveur MCP d'envoi de notifications ntfy";
      license = licenses.gpl3;
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
    readIfPossible = f:
      let e = builtins.tryEval (builtins.readFile f);
      in if e.success then e.value else "";
    envFiles = cfg.environmentFiles
      ++ lib.optional (cfg.tokenFile != null) cfg.tokenFile
      ++ lib.optional (cfg.clientTokenFile != null) cfg.clientTokenFile;
    clientTokenProvided = lib.any
      (f: lib.hasInfix "MCP_NTFY_CLIENT_TOKEN=" (readIfPossible f))
      envFiles;
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

      environmentFiles = lib.mkOption {
        type = lib.types.listOf lib.types.path;
        default = [ ];
        description = ''
    Fichiers d'environnement (une ligne KEY=VALUE par secret) chargés par
    le service systemd, ex. [ "/run/secrets/ntfy-mcp-token" ] pour
    NTFY_TOKEN=... et [ "/run/secrets/ntfy-mcp-client-token" ] pour
    MCP_NTFY_CLIENT_TOKEN=... (obligatoire quand le serveur écoute hors
    loopback : toute requête MCP doit alors porter ce secret en en-tête
    "Authorization: Bearer ..."). Préféré aux secrets en clair dans la
    configuration.
        '';
      };

      tokenFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        description = ''
    (Obsolète) Fichier contenant le jeton d'accès ntfy, au format
    NTFY_TOKEN=... (une ligne). Remplacé par environmentFiles ; supprimé
    dans une version x.x.0.
        '';
      };

      clientTokenFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        description = ''
    (Obsolète) Fichier contenant MCP_NTFY_CLIENT_TOKEN=... (une ligne).
    Remplacé par environmentFiles ; supprimé dans une version x.x.0.
        '';
      };

      allowPrivateUpstream = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = ''
    Autorise les connexions montantes vers des IP privées/loopback
    (MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1). À n'activer que pour un serveur
    ntfy auto-hébergé sur le réseau local ; par défaut, ces adresses sont
    refusées au moment de la connexion (protection anti-SSRF).
        '';
      };

      trustedProxies = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        description = ''
          IPs/CIDRs de proxys inverses dont l'en-tête X-Forwarded-For est
          pris en compte pour les limites de débit par client
          (MCP_NTFY_TRUSTED_PROXIES). À renseigner quand le serveur est
          derrière un proxy inverse sur la même machine, ex.
          [ "127.0.0.1/32" ] avec un nginx local.
        '';
      };

      allowedHosts = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        description = ''
          Valeurs publiques de l'en-tête Host acceptées sur /mcp quand le
          serveur écoute sur localhost (MCP_NTFY_ALLOWED_HOSTS), ex.
          [ "ntfy-mcp.example.com" ]. Le transport MCP refuse les en-têtes
          Host non-localhost sur une connexion loopback (protection
          anti-DNS-rebinding) : cette option réécrit l'en-tête Host
          correspondant en adresse loopback. Alternative à
          "proxy_set_header Host 127.0.0.1;" dans le proxy inverse,
          à utiliser quand on ne contrôle pas le proxy.
        '';
      };
    };

    config = lib.mkIf cfg.enable {
      warnings =
        lib.optional (cfg.tokenFile != null || cfg.clientTokenFile != null)
          "ntfy-mcp: services.ntfy-mcp.tokenFile and services.ntfy-mcp.clientTokenFile are deprecated; use services.ntfy-mcp.environmentFiles. The old options will be removed in a x.x.0 release."
        ++ lib.optional
          (!(builtins.elem cfg.address [ "127.0.0.1" "::1" "localhost" ]) && !clientTokenProvided)
          "ntfy-mcp: address is not loopback and no environment file provides MCP_NTFY_CLIENT_TOKEN: the MCP server will be reachable without client authentication; add a MCP_NTFY_CLIENT_TOKEN=... file to environmentFiles or expose it only behind an authenticated TLS reverse proxy.";

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
    ]
    ++ lib.optional cfg.allowPrivateUpstream "MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1"
    ++ lib.optional (cfg.trustedProxies != [ ]) "MCP_NTFY_TRUSTED_PROXIES=${lib.concatStringsSep "," cfg.trustedProxies}"
    ++ lib.optional (cfg.allowedHosts != [ ]) "MCP_NTFY_ALLOWED_HOSTS=${lib.concatStringsSep "," cfg.allowedHosts}";
    EnvironmentFile = envFiles;
    DynamicUser = true;
    NoNewPrivileges = true;
    PrivateTmp = true;
    PrivateDevices = true;
    ProtectSystem = "strict";
    ProtectHome = true;
    ProtectKernelTunables = true;
    ProtectKernelModules = true;
    ProtectKernelLogs = true;
    ProtectClock = true;
    ProtectHostname = true;
    ProtectControlGroups = true;
    RestrictNamespaces = true;
    RestrictRealtime = true;
    RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" ];
    CapabilityBoundingSet = "";
    AmbientCapabilities = "";
    LockPersonality = true;
    MemoryDenyWriteExecute = true;
    RemoveIPC = true;
    SystemCallFilter = [ "@system-service" ];
    SystemCallErrorNumber = "EPERM";
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
