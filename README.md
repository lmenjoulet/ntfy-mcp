# ntfy-mcp

A [Model Context Protocol (MCP)](https://github.com/modelcontextprotocol/spec) server that exposes a tool to send notifications via [ntfy](https://ntfy.sh/).

## Overview

This server allows MCP clients (like AI agents) to send notifications to an ntfy server. Useful for:
- Receiving alerts when scheduled tasks complete
- Getting notifications for long-running operations
- Integrating notifications into AI agent workflows

## Configuration

Configure the server using environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `NTFY_URL` | `https://ntfy.sh` | Base URL of the ntfy server |
| `NTFY_TOPIC` | `agent` | Default topic for notifications |
| `NTFY_TOKEN` | *empty* | Authentication token for the ntfy server |
| `MCP_NTFY_PORT` | `12120` | Port to listen on for MCP connections |

## Usage

### Running the server

```bash
# Using environment variables (optional)
export NTFY_URL=https://ntfy.sh
export NTFY_TOPIC=my-app
export NTFY_TOKEN=your_token_here
export MCP_NTFY_PORT=12120

# Start the server
./ntfy-mcp
```

The server listens on `http://localhost:12120/mcp` by default.

### MCP Tool: `send_notification`

Sends a notification to an ntfy server.

**Required parameters:**
- `token` - Authentication token for the ntfy server
- `message` - Body of the notification
- `topic` - Destination topic

**Optional parameters:**
- `base_url` - Override the ntfy server URL (defaults to `NTFY_URL`)
- `title` - Notification title
- `priority` - Priority level (1-5, where 1 is lowest, 5 is highest)
- `tags` - Comma-separated tags (e.g., `"warning,phone"`)

## Building

### With Go

```bash
git clone https://github.com/lmenjoulet/ntfy-mcp
cd ntfy-mcp
go build -o ntfy-mcp
```

### With Nix

```bash
nix build .
./result/bin/ntfy-mcp
```

## Logging

The server logs to stderr with the `[ntfy-mcp]` prefix. Tokens are automatically masked in logs.

## License

MIT
