# ntfy-mcp

A [Model Context Protocol (MCP)](https://github.com/modelcontextprotocol/spec) server that exposes a tool to send notifications via [ntfy](https://ntfy.sh/).  
Its goal is to be lightweight and act as the glue for agents running in harnesses that don't give them full HTTP client capabilities.

## Overview

This server allows MCP clients (usually AI agents) to send notifications to an [ntfy](https://ntfy.sh) server.

## Configuration

Configure the server using environment variables:


| Variable               | Default           | Description                                                |
| ---------------------- | ----------------- | ---------------------------------------------------------- |
| `NTFY_URL`             | `https://ntfy.sh` | Base URL of the ntfy server                                |
| `NTFY_TOPIC`           | `agent`           | Default topic for notifications                            |
| `NTFY_TOKEN`           | *empty*           | Authentication token for the ntfy server                   |
| `MCP_NTFY_ADDR`        | `127.0.0.1`       | Address to listen on for MCP connections                   |
| `MCP_NTFY_PORT`        | `12120`           | Port to listen on for MCP connections                      |
| `MCP_NTFY_UNSAFE_LOGS` | `0`               | Log level for unsafe logging; `1` logs tokens in cleartext |


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
  address = "127.0.0.1";   # listen address
  port = 12120;
};
```

## MCP capabilities

- Send notifications
- **TODO** Read notifications

## Security considerations

### Setting variables

The server supports two deployment regimes, depending on whether the `NTFY_TOKEN` environment variable is set:

- **Private server**: the token stays server-side (`NTFY_TOKEN`) and agents only provide `message`, `title`, `priority` and `tags`. The agent never sees any credential and cannot choose the ntfy server URL (to prevent token exfiltration).
  - **TODO** implement a security mecanism to prevent unauthenticated users to spam notifications.
- **Public server**: agents provide `base_url` and `token` themselves, which allows multiplexing several ntfy backends through one MCP server.

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
