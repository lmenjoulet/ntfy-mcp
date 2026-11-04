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

- ports are configurable via `FAKE_NTFY_PORT` and `MCP_TEST_PORT`;
- no interaction, no external services;
- standard exit codes (`0` pass, `1` fail).

## Security rules

- Never log tokens in cleartext. Logs redact tokens by default;
  `MCP_NTFY_UNSAFE_LOGS=1` is for local debugging only.
- Any new tool that takes a `base_url` must keep the anti-exfiltration
  guard: a custom `base_url` requires a token in the same request
  (see `config.resolveTarget` in `main.go`). The test suite covers this.
- Test scripts must not contain real secrets.
