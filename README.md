# ntfy-mcp

[![CI (tests + govulncheck)](https://github.com/lmenjoulet/ntfy-mcp/actions/workflows/ci.yaml/badge.svg)](https://github.com/lmenjoulet/ntfy-mcp/actions/workflows/ci.yaml)
[![Security (govulncheck)](https://img.shields.io/badge/govulncheck-passing-brightgreen)](https://github.com/lmenjoulet/ntfy-mcp/actions/workflows/ci.yaml)
[![Go](https://img.shields.io/github/go-mod/go-version/lmenjoulet/ntfy-mcp?logo=go&logoColor=white)](https://go.dev/)
[![License: GPL-3.0-or-later](https://img.shields.io/badge/license-GPL--3.0--or--later-blue.svg)](LICENSE)

A [Model Context Protocol (MCP)](https://github.com/modelcontextprotocol/spec) server that lets MCP clients (usually AI agents) send notifications via [ntfy](https://ntfy.sh) — lightweight glue for agent harnesses without full HTTP client capabilities.

## Usage

### Running the server

```bash
export NTFY_TOKEN=your_token_here
./ntfy-mcp
```

By default the server listens on loopback only, at `http://127.0.0.1:12120/mcp`. Set `MCP_NTFY_ADDR` to expose it — behind an authenticated reverse proxy (see [Security](#security-considerations)).

The HTTP transport also serves `GET /healthz` (answers `200 ok`), for liveness/readiness probes. When `MCP_NTFY_CLIENT_TOKEN` is set, `/healthz` requires the Bearer token like every other endpoint — pass it via the probe's `httpHeaders`. On SIGTERM/SIGINT the server stops accepting new connections, drains in-flight requests (up to 10 s) and exits 0, so it is safe to run under systemd, Kubernetes or Docker.

### stdio transport

Set `MCP_NTFY_TRANSPORT=stdio` to serve MCP over stdin/stdout instead of HTTP — the usual way MCP clients spawn a local server. In stdio mode `MCP_NTFY_ADDR`, `MCP_NTFY_PORT` and `MCP_NTFY_CLIENT_TOKEN` have no effect.

```json
{
  "mcpServers": {
    "ntfy": {
      "command": "/path/to/ntfy-mcp",
      "env": {
        "MCP_NTFY_TRANSPORT": "stdio",
        "NTFY_URL": "https://ntfy.sh",
        "NTFY_TOPIC": "my-app",
        "NTFY_TOKEN": "your_token_here"
      }
    }
  }
}
```

### Environment variables

See [docs/environment-variables.md](docs/environment-variables.md).

### Running as a NixOS service

The flake exposes a NixOS module:

```nix
{
  inputs.ntfy-mcp.url = "github:lmenjoulet/ntfy-mcp";

  outputs = { nixpkgs, ntfy-mcp, ... }: {
    nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [ ./configuration.nix ntfy-mcp.nixosModules.default ];
    };
  };
}
```

Example configuration with an nginx reverse proxy.
```nix
{
  services.ntfy-mcp = {
    enable = true;
    address = "127.0.0.1";   # loopback only; nginx exposes it
    port = 12120;
    ntfyUrl = "https://ntfy.sh";
    ntfyTopic = "my-topic";
    environmentFiles = [
      "/run/secrets/ntfy-mcp-token";        # one NTFY_TOKEN=... line
      "/run/secrets/ntfy-mcp-client-token"; # one MCP_NTFY_CLIENT_TOKEN=... line
    ];
    trustedProxies = [ "127.0.0.1/32" ]; # trust nginx's X-Forwarded-For for per-client rate limits
    # allowedHosts = [ "ntfy-mcp.example.com" ]; # accept this public Host on /mcp (alternative to "proxy_set_header Host 127.0.0.1;" in nginx)
    # allowPrivateUpstream = true;      # self-hosted ntfy on the LAN only
  };

  services.nginx = {
    enable = true;
    virtualHosts."ntfy-mcp.example.com" = {
      forceSSL = true;
      locations."/mcp" = {
        proxyPass = "http://127.0.0.1:12120";
        extraConfig = ''
          proxy_http_version 1.1;
          # mcp-go rejects requests whose Host header is not loopback when the
          # server listens on 127.0.0.1 (DNS-rebinding protection): rewrite it.
          proxy_set_header Host 127.0.0.1;
          proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
          proxy_buffering off;
          proxy_read_timeout 300s;
        '';
      };
    };
  };
}
```

## MCP capabilities

- Send notifications, with optional `title`, `priority`, `tags`, `click` (URL opened on click), `icon`, `delay` (deferred delivery), `email` (forwarding) and `markdown` rendering
- Read cached notifications from a topic

## Security considerations

### Deployment regimes

The server supports two regimes, depending on whether `NTFY_TOKEN` is set:

- **Private server** (default): the token stays server-side and agents only provide `message`, `title`, `priority` and `tags` — they never see a credential and cannot choose the upstream ntfy URL.
- **Public server**: agents provide `base_url` and `token` themselves and authenticate against the target ntfy server with their own token, so one MCP server can multiplex several ntfy backends.

### Exposing the server

The server listens on loopback by default and speaks plain HTTP. Expose it beyond loopback only behind a **TLS-terminating reverse proxy** and set `MCP_NTFY_CLIENT_TOKEN`: every MCP request must then carry it as `Authorization: Bearer <token>` and is rejected with `401` otherwise, so unauthenticated users cannot spam notifications through your topic.

nginx example :

```nginx
server {
    listen 443 ssl;
    server_name ntfy-mcp.example.com;

    location /mcp {
        proxy_pass http://127.0.0.1:12120;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        # forwarded so the server can rate-limit per real client IP
        # (requires MCP_NTFY_TRUSTED_PROXIES, see below):
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        # MCP streamable HTTP uses long-lived SSE responses:
        proxy_buffering off;
        proxy_read_timeout 300s;
    }
}
```

With a proxy on the same host, also set `MCP_NTFY_TRUSTED_PROXIES=127.0.0.1/32` so the per-client rate limits use the real client IP from `X-Forwarded-For` instead of the proxy's address.

### Built-in protections

In the public-server regime the server refuses to act as an open proxy to the internal network: upstream targets on private/loopback/link-local IPs (including cloud metadata endpoints) are rejected by default (anti-SSRF — set `MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1` only for a self-hosted ntfy on the LAN, never on an Internet-exposed endpoint), and a target that repeatedly answers `401`/`403`/`429` is temporarily blocked without any upstream call (anti proxy-DoS).

Per-IP rate limits, request/body size caps, message size limits and upstream concurrency caps are also enforced by default; all tunables are in the environment table above. Implementation details live in `main.go` — reading the source is the reference.

### Errors and logging

Only errors caused by the client's own input are returned to the MCP client; internal errors and upstream response bodies are logged server-side and never leak to clients.

Logs go to stderr with the `[ntfy-mcp]` prefix, with tokens redacted as `[redacted]` by default. `MCP_NTFY_UNSAFE_LOGS=1` logs tokens in cleartext for debugging — use it only temporarily, never in production.

## Building

```bash
git clone https://github.com/lmenjoulet/ntfy-mcp && cd ntfy-mcp
go build   # with Go on PATH
nix build  # release build via the flake
```

<!-- TODO: Add the server to common MCP registries -->

## License

This project is licensed under the GNU General Public License v3.0 or later (GPL-3.0-or-later) — see [LICENSE](LICENSE) for the full text of version 3.

Copyright (C) 2026 Loup Menjoulet
