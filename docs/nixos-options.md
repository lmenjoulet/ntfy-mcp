# NixOS module options

The flake's NixOS module (`nixosModules.default`) runs ntfy-mcp as a hardened systemd service. All options of `services.ntfy-mcp`:

| Option | Default | Description |
| --- | --- | --- |
| `enable` | `false` | Start the ntfy-mcp service |
| `package` | the flake's `ntfy-mcp` package | Package to run |
| `address` | `127.0.0.1` | Listen address (`MCP_NTFY_ADDR`). Loopback by default: expose beyond loopback only behind an authenticated TLS reverse proxy |
| `port` | `12120` | Listen port (`MCP_NTFY_PORT`); MCP endpoint at `/mcp` |
| `ntfyUrl` | `https://ntfy.sh` | Default ntfy server URL (`NTFY_URL`) |
| `ntfyTopic` | `agent` | Default ntfy topic (`NTFY_TOPIC`) |
| `environmentFiles` | `[ ]` | Environment files loaded by the service (systemd `EnvironmentFile=`), one `KEY=VALUE` per line, e.g. `/run/secrets/ntfy-mcp-token` containing `NTFY_TOKEN=...`. Provide `MCP_NTFY_CLIENT_TOKEN=...` in one of them when listening beyond loopback (mandatory there). Prefer this over secrets in plaintext in `configuration.nix` |
| `allowPrivateUpstream` | `false` | Sets `MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1`: allow an upstream ntfy server on a private/loopback IP (self-hosted ntfy on the LAN only, never on an Internet-exposed endpoint) |
| `trustedProxies` | `[ ]` | Sets `MCP_NTFY_TRUSTED_PROXIES`: proxy IPs/CIDRs whose `X-Forwarded-For` is trusted for per-client rate limits, e.g. `[ "127.0.0.1/32" ]` behind a local nginx |
| `allowedHosts` | `[ ]` | Sets `MCP_NTFY_ALLOWED_HOSTS`: public Host values accepted on `/mcp` when listening on loopback, e.g. `[ "ntfy-mcp.example.com" ]` when the reverse proxy keeps the public Host |

Options that map to an environment variable are documented in [environment-variables.md](environment-variables.md).
