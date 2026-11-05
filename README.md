# ntfy-mcp

[![CI (tests + govulncheck)](https://github.com/lmenjoulet/ntfy-mcp/actions/workflows/ci.yaml/badge.svg)](https://github.com/lmenjoulet/ntfy-mcp/actions/workflows/ci.yaml)
[![Security (govulncheck)](https://img.shields.io/badge/govulncheck-passing-brightgreen)](https://github.com/lmenjoulet/ntfy-mcp/actions/workflows/ci.yaml)
[![Go](https://img.shields.io/github/go-mod/go-version/lmenjoulet/ntfy-mcp?logo=go&logoColor=white)](https://go.dev/)
[![License: GPL-3.0-or-later](https://img.shields.io/badge/license-GPL--3.0--or--later-blue.svg)](LICENSE)

A [Model Context Protocol (MCP)](https://github.com/modelcontextprotocol/spec) server that exposes a tool to send notifications via [ntfy](https://ntfy.sh/).  
Its goal is to be lightweight and act as the glue for agents running in harnesses that don't give them full HTTP client capabilities.

## Overview

This server allows MCP clients (usually AI agents) to send notifications to an [ntfy](https://ntfy.sh) server.

## Usage

### Running the server

```bash
# Using environment variables (optional)
export NTFY_URL=https://ntfy.sh
export NTFY_TOPIC=my-app
export NTFY_TOKEN=your_token_here
export MCP_NTFY_ADDR=127.0.0.1
export MCP_NTFY_PORT=12120

# Start the server
./ntfy-mcp
```

By default the server only listens on loopback, at `http://127.0.0.1:12120/mcp`. Set `MCP_NTFY_ADDR=0.0.0.0` (or a specific interface address) to expose it — in that case, put it behind an authenticated reverse proxy.

### stdio transport

Set `MCP_NTFY_TRANSPORT=stdio` to serve MCP over stdin/stdout (newline-delimited JSON-RPC) instead of HTTP — the usual way MCP clients such as Claude Desktop spawn a local server. Logs stay on stderr, so they never pollute the protocol channel. In this mode `MCP_NTFY_ADDR`, `MCP_NTFY_PORT` and `MCP_NTFY_CLIENT_TOKEN` have no effect (there are no HTTP headers to authenticate).

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

Configure the server using environment variables:


| Variable               | Default           | Description                                                |
| ---------------------- | ----------------- | ---------------------------------------------------------- |
| `NTFY_URL`             | `https://ntfy.sh` | Base URL of the ntfy server                                |
| `NTFY_TOPIC`           | `agent`           | Default topic for notifications                            |
| `NTFY_TOKEN`           | *empty*           | Authentication token for the ntfy server                   |
| `MCP_NTFY_TRANSPORT`   | `http`            | Transport: `http` (listen on ADDR:PORT) or `stdio` (MCP over stdin/stdout) |
| `MCP_NTFY_ADDR`        | `127.0.0.1`       | Address to listen on for MCP connections (http transport only) |
| `MCP_NTFY_PORT`        | `12120`           | Port to listen on for MCP connections (http transport only)   |
| `MCP_NTFY_CLIENT_TOKEN` | *empty*          | Shared secret MCP clients must send as `Authorization: Bearer <token>`; when set, unauthenticated requests are rejected with `401` |
| `MCP_NTFY_RATE_LIMIT_FAILURES` | `5`     | Upstream rejections (401/403/429) per target base URL tolerated within the window before the target is blocked; `0` disables the guard |
| `MCP_NTFY_RATE_LIMIT_WINDOW` | `60`       | Sliding window, in seconds, for the upstream guard **and** the client rate limits below |
| `MCP_NTFY_RATE_LIMIT_BLOCK` | `300`       | How long a target base URL stays blocked, in seconds              |
| `MCP_NTFY_CLIENT_RATE_LIMIT` | `120`    | Max MCP requests per client IP within the window; over the limit → `429`; `0` disables |
| `MCP_NTFY_AUTH_FAILURE_LIMIT` | `10`     | Max failed client authentications per IP within the window before `429` (anti brute force) |
| `MCP_NTFY_MAX_CONCURRENT_UPSTREAM` | `32` | Max concurrent calls to upstream ntfy servers |
| `MCP_NTFY_ALLOW_PRIVATE_UPSTREAM` | `0`   | `1` allows upstream connections to private/loopback/link-local IPs (self-hosted ntfy on the LAN only); refused by default (anti-SSRF) |
| `MCP_NTFY_UNSAFE_LOGS` | `0`               | `1` logs tokens in cleartext for debugging; startup warning if combined with a non-loopback `MCP_NTFY_ADDR` |


### Running as a NixOS service

The flake exposes a NixOS module that runs the server as a systemd service.

```nix
{
  inputs = {
    # ...
    ntfy-mcp.url = "github:lmenjoulet/ntfy-mcp";
   #ntfy-mcp.inputs.nixpkgs.follows = "nixpkgs"; 
   #to avoid duplicate inputs, if unable to build keep it disabled.
  };

  outputs = { self, nixpkgs, ntfy-mcp }: {
    nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        ./configuration.nix
        ntfy-mcp.nixosModules.default
      ];
    };
  };
}
```

Then enable and configure the service:

```nix
services.ntfy-mcp = {
  enable = true;
  ntfyUrl = "https://ntfy.sh";
  ntfyTopic = "my-topic";
  # Token kept out of the Nix store, in a file containing one
  # NTFY_TOKEN=... line:
  tokenFile = "/run/secrets/ntfy-mcp-token";
  # Optional: shared secret required from MCP clients (anti-spam when
  # the server is exposed beyond loopback), in a file containing one
  # MCP_NTFY_CLIENT_TOKEN=... line:
  # clientTokenFile = "/run/secrets/ntfy-mcp-client-token";
  address = "127.0.0.1";   # listen address
  port = 12120;
  # Only for a self-hosted ntfy on the local network: allows upstream
  # connections to private/loopback IPs (refused by default, anti-SSRF):
  # allowPrivateUpstream = true;
};
```

A NixOS evaluation warning is emitted when `address` is not loopback and `clientTokenFile` is not set. The service runs hardened (DynamicUser, `NoNewPrivileges`, `ProtectSystem=strict`, capability bounding set emptied, syscall filter, …).

## MCP capabilities

- Send notifications
- Read cached notifications from a topic (`read_notifications`: optional `since`, `limit`, `cached_only`)

## Security considerations

### Deployment regimes

The server supports two regimes, depending on whether `NTFY_TOKEN` is set:

- **Private server** (default): the token stays server-side and agents only provide `message`, `title`, `priority` and `tags` — they never see a credential and cannot choose the upstream ntfy URL.
- **Public server**: agents provide `base_url` and `token` themselves and authenticate against the target ntfy server with their own token, so one MCP server can multiplex several ntfy backends.

### Exposing the server

The server listens on loopback by default and speaks plain HTTP. Expose it beyond loopback only behind a **TLS-terminating reverse proxy** (nginx, Caddy, Traefik, …) and set `MCP_NTFY_CLIENT_TOKEN`: every MCP request must then carry it as `Authorization: Bearer <token>` and is rejected with `401` otherwise, so unauthenticated users cannot spam notifications through your topic.

### Built-in protections

In the public-server regime the server refuses to act as an open proxy to the internal network: upstream targets on private/loopback/link-local IPs (including cloud metadata endpoints) are rejected by default (anti-SSRF — set `MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1` only for a self-hosted ntfy on the LAN, never on an Internet-exposed endpoint), and a target that repeatedly answers `401`/`403`/`429` is temporarily blocked without any upstream call (anti proxy-DoS).

Per-IP rate limits, request/body size caps, message size limits and upstream concurrency caps are also enforced by default; all tunables are in the environment table above. Implementation details live in `main.go` — reading the source is the reference.

### Errors and logging

Only errors caused by the client's own input are returned to the MCP client; internal errors and upstream response bodies are logged server-side and never leak to clients.

Logs go to stderr with the `[ntfy-mcp]` prefix, with tokens redacted as `[redacted]` by default. `MCP_NTFY_UNSAFE_LOGS=1` logs tokens in cleartext for debugging — use it only temporarily, never in production (a startup warning is emitted if combined with a non-loopback `MCP_NTFY_ADDR`).

## Building

### With Go

```bash
git clone https://github.com/lmenjoulet/ntfy-mcp
cd ntfy-mcp
go build
```

### With Nix

```bash
git clone https://github.com/lmenjoulet/ntfy-mcp
cd ntfy-mcp
# nix develop # Optional: enter development environment with necessary dependencies installed
nix build
```

The nix flake file has additional build options as it is intended for release builds.

**TODO** Add the server on common MCP registries

## License

This project is licensed under the GNU General Public License v3.0 or later (GPL-3.0-or-later) — see [LICENSE](LICENSE) for the full text of version 3.

Copyright (C) 2026 Loup Menjoulet
