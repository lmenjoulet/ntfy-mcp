# ntfy-mcp

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
| `MCP_NTFY_ALLOW_PRIVATE_UPSTREAM` | `0`   | `1` allows upstream connections to private/loopback/link-local IPs (self-hosted ntfy on the LAN only); by default such IPs are refused at dial time (anti-SSRF) |
| `MCP_NTFY_UNSAFE_LOGS` | `0`               | Log level for unsafe logging; `1` logs tokens in cleartext (a startup warning is emitted if combined with a non-loopback `MCP_NTFY_ADDR`) |


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

### Setting variables

The server supports two deployment regimes, depending on whether the `NTFY_TOKEN` environment variable is set:

- **Private server**: the token stays server-side (`NTFY_TOKEN`) and agents only provide `message`, `title`, `priority` and `tags`. The agent never sees any credential and cannot choose the ntfy server URL (to prevent token exfiltration).
- **Public server**: agents provide `base_url` and `token` themselves — clients authenticate against the *target* ntfy server with their own token, so `MCP_NTFY_CLIENT_TOKEN` is not needed there (it stays optional, and only restricts who may use the MCP proxy at all). This allows multiplexing several ntfy backends through one MCP server.

### MCP client authentication (anti-spam)

By default the server only listens on loopback. If you expose it more widely (`MCP_NTFY_ADDR=0.0.0.0`, or through a reverse proxy that does not authenticate clients), set `MCP_NTFY_CLIENT_TOKEN` to a shared secret: every MCP request must then carry it in an `Authorization: Bearer <token>` header, and any request without a valid token is rejected with `401 Unauthorized` before reaching the tools. This prevents unauthenticated users from spamming notifications through your ntfy topic. The comparison is constant-time and the presented token is never logged.

```bash
export MCP_NTFY_CLIENT_TOKEN=another_long_random_secret
./ntfy-mcp
# clients then call the endpoint with:
#   Authorization: Bearer another_long_random_secret
```

When `MCP_NTFY_CLIENT_TOKEN` is empty (the default), no client authentication is performed — keep the server on loopback or behind an authenticated reverse proxy in that case.

The server itself speaks plain HTTP. When exposing it beyond loopback, always put it behind a **TLS-terminating reverse proxy** (nginx, Caddy, Traefik, …) so the `MCP_NTFY_CLIENT_TOKEN` Bearer header and the notification content are never sent in cleartext on the network.

### Upstream rate limiting (anti proxy-DoS)

In the public-server regime, a client could otherwise use this MCP server as a proxy to flood a third-party ntfy server (e.g. by sending many requests that the target rejects with `401`). To prevent that, the server keeps an **upstream guard**: when a target base URL accumulates `MCP_NTFY_RATE_LIMIT_FAILURES` rejection responses (`401`/`403`/`429`) within `MCP_NTFY_RATE_LIMIT_WINDOW` seconds, that target is blocked for `MCP_NTFY_RATE_LIMIT_BLOCK` seconds and requests to it are refused locally, **without any upstream call**.

The guard is keyed by target base URL only, not by request token, so rotating tokens cannot evade the block. Set `MCP_NTFY_RATE_LIMIT_FAILURES=0` to disable it.

### Upstream SSRF protection

In the public-server regime (client-provided `base_url` + `token`), the server must never become an open proxy to the internal network. Two layers apply to every upstream connection:

- **Scheme and shape**: `base_url` must be an absolute `http`/`https` URL without embedded credentials; anything else is rejected. The `topic` argument is restricted to the ntfy charset (`[A-Za-z0-9_-]{1,64}`), so it cannot inject path segments (`../`), query (`?`) or fragment (`#`) into the upstream URL.
- **Dial-time IP check**: loopback, private (RFC 1918 + ULA), link-local (including the cloud metadata endpoint `169.254.169.254`), CGNAT, multicast and unspecified addresses are refused when the connection is established, on the *resolved* addresses, and the connection is pinned to a validated IP — so DNS rebinding cannot bypass it. TLS certificate verification is unaffected (ServerName still comes from the URL).

Set `MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1` only if your ntfy server is self-hosted on a private/loopback address; this weakens the protection and should never be combined with an Internet-exposed MCP endpoint.

### Rate limiting and resource caps (anti-DoS)

- Per client IP (direct peer; `X-Forwarded-For` is deliberately not trusted): max `MCP_NTFY_CLIENT_RATE_LIMIT` MCP requests per `MCP_NTFY_RATE_LIMIT_WINDOW` seconds (`429` when exceeded), and max `MCP_NTFY_AUTH_FAILURE_LIMIT` failed authentications per window before `429` (anti brute force on `MCP_NTFY_CLIENT_TOKEN`).
- MCP request bodies are capped at 1 MiB; the HTTP server sets read/write/idle timeouts so stalled clients cannot hold connections forever.
- `message` is capped at 4096 bytes (the ntfy server limit).
- `read_notifications` reads at most `limit` messages (default 100, hard cap 1000) and at most 4 MiB of stream.
- At most `MCP_NTFY_MAX_CONCURRENT_UPSTREAM` upstream calls run concurrently.
- The upstream guard's internal maps are size-capped, so spraying unique `base_url` values cannot exhaust memory.

### Error handling and logging hygiene

Errors caused by the client's own input (invalid `base_url`/`topic`, oversized `message`, temporarily blocked target) are returned explicitly to the MCP client. All other errors (network failures, upstream status/body, internal details) are logged server-side only and replaced by a generic message, so internal details and upstream response bodies never leak to MCP clients. Client-controlled fields are logged with `%q` to prevent log forging.

### Logging

The server logs to stderr with the `[ntfy-mcp]` prefix. By default, tokens are replaced with `[redacted]` in log lines, which can be an annoyance when troubleshooting connection issues.

For debugging, set `MCP_NTFY_UNSAFE_LOGS=1` to log authentication tokens in cleartext.

```bash
MCP_NTFY_UNSAFE_LOGS=1 ./ntfy-mcp
```

Logs are not a secret-safe place: never enable this on a production or multi-user system, and only temporarily while diagnosing authentication failures.

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
