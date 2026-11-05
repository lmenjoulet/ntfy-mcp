# AGENTS.md — instructions for agents working on ntfy-mcp

## Build

Always build through the Nix dev shell (Go is provided there):

```bash
nix develop --command go build ./...
nix build          # full release build via the flake
```

## Tests — mandatory before every commit

```bash
nix develop --command bash scripts/run-tests.sh
```

- `scripts/run-tests.sh` is a **hermetic** end-to-end suite: it builds the
  server from the current sources, starts a fake ntfy server
  (`scripts/fake-ntfy.go`) and the MCP server on localhost, and exercises
  the MCP tools over HTTP. No real ntfy server, token, or external network
  access is required.
- Exit code `0` = all tests passed. **Never commit when the suite fails.**
- If a test fails, the server logs are kept and their path is printed at
  the end of the output — read them before changing anything.

## Manual smoke test (optional, real ntfy server)

```bash
TEST_NTFY_TOKEN=tk_... bash scripts/test-server.sh
```

This sends an actual notification through a real ntfy server. Do not use
it in CI and never put a real token in a committed file.

## CI/CD integration

`scripts/run-tests.sh` is designed to run unchanged in a classic CI
pipeline (e.g. GitHub Actions with `actions/setup-go` — no Nix needed,
just Go on `PATH`):

- ports are configurable via `FAKE_NTFY_PORT`, `MCP_TEST_PORT` and
  `MCP_AUTH_TEST_PORT`;
- no interaction, no external services;
- standard exit codes (`0` pass, `1` fail).

## Documentation

- The README is user-facing documentation: what the project is, how to run,
  configure and deploy it. It must not explain the internal implementation —
  reading the source code should be sufficient. If a README section describes
  how the code works internally (mechanisms, data flow, guard internals),
  reduce it to the operator-facing consequence or remove it, and keep the
  README short.
- Put implementation rationale in code comments, not in the README.

## Security rules

- Never log tokens in cleartext. Logs redact tokens by default;
  `MCP_NTFY_UNSAFE_LOGS=1` is for local debugging only. This applies to
  `MCP_NTFY_CLIENT_TOKEN` too: the middleware in `main.go`
  (`withClientAuth`) must never log the presented token.
- When exposing the server beyond loopback, require
  `MCP_NTFY_CLIENT_TOKEN` (anti-spam client authentication); the test
  suite covers the 401 paths.
- Any code path that forwards a request to an upstream ntfy server must
  go through the anti-DoS guard (`config.checkUpstreamAllowed` before the
  HTTP call, `config.noteUpstreamStatus` with the response status after
  it — see `upstreamGuard` in `main.go`). The test suite covers the block
  and its expiry.
- Any new tool that takes a `base_url` must keep the anti-exfiltration
  guard: a custom `base_url` requires a token in the same request
  (see `config.resolveTarget` in `main.go`). The test suite covers this.
- Any new tool that takes a `base_url` must also go through
  `validateBaseURL` (http/https only, no embedded credentials) and the
  dial-time IP guard in `safeDialContext` (private/loopback/link-local/
  metadata IPs refused unless `MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1`) —
  the shared `httpClient` applies the latter automatically, keep using it.
- Never return upstream response bodies or internal error details to
  MCP clients: only `toolInputError` messages may cross back (see the
  handlers in `main.go`). Log full detail server-side with `%q` on
  client-controlled fields.
- Test scripts must not contain real secrets.
