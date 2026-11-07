# Environment variables

| Variable | Default | Description |
| --- | --- | --- |
| `NTFY_URL` | `https://ntfy.sh` | Base URL of the ntfy server |
| `NTFY_TOPIC` | `agent` | Default topic |
| `NTFY_TOKEN` | *empty* | ntfy server auth token |
| `MCP_NTFY_TRANSPORT` | `http` | `http` or `stdio` |
| `MCP_NTFY_ADDR` | `127.0.0.1` | Listen address (`http` only) |
| `MCP_NTFY_PORT` | `12120` | Listen port (`http` only) |
| `MCP_NTFY_CLIENT_TOKEN` | *empty* | Bearer token required from MCP clients (`401` otherwise); required when exposing the server beyond loopback |
| `MCP_NTFY_RATE_LIMIT_FAILURES` | `5` | Upstream 401/403/429 per base URL before it is blocked; `0` disables |
| `MCP_NTFY_RATE_LIMIT_WINDOW` | `60` | Sliding window in seconds for the guard and the client rate limits |
| `MCP_NTFY_RATE_LIMIT_BLOCK` | `300` | Block duration in seconds for a rejected base URL |
| `MCP_NTFY_CLIENT_RATE_LIMIT` | `120` | Max requests per client IP per window (`429` over); `0` disables |
| `MCP_NTFY_AUTH_FAILURE_LIMIT` | `10` | Max failed authentications per IP per window (`429` over) |
| `MCP_NTFY_MAX_CONCURRENT_UPSTREAM` | `32` | Max concurrent upstream calls |
| `MCP_NTFY_TRUSTED_PROXIES` | *empty* | Comma-separated proxy IPs/CIDRs whose `X-Forwarded-For` is trusted for the per-client rate limits (set it behind a reverse proxy you control) |
| `MCP_NTFY_ALLOWED_HOSTS` | *empty* | Comma-separated public Host values accepted on `/mcp` when listening on loopback; use when a reverse proxy keeps the public Host. Prefer `proxy_set_header Host 127.0.0.1;` when you control the proxy |
| `MCP_NTFY_ALLOW_PRIVATE_UPSTREAM` | `0` | `1` allows private/loopback upstream IPs (LAN-hosted ntfy only, never on an Internet-exposed endpoint) |
| `MCP_NTFY_UNSAFE_LOGS` | `0` | `1` logs tokens in cleartext (debug only, never in production) |

Invalid values (port outside 1-65535, negative limits, malformed `NTFY_TOPIC`, `MCP_NTFY_ADDR` or `MCP_NTFY_ALLOWED_HOSTS` entry) are rejected at startup with an error naming the variable. A `NTFY_URL` that is or resolves to a private/loopback address is also rejected at startup unless `MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1`.
