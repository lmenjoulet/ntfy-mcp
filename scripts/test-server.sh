#!/usr/bin/env bash
# Manual smoke test against a REAL ntfy server (not for CI: it sends an
# actual notification). For the automated hermetic suite, use
# scripts/run-tests.sh instead.
#
# Usage:
#   TEST_NTFY_TOKEN=tk_... bash scripts/test-server.sh
#
# Optional environment variables:
#   TEST_NTFY_SERVER   ntfy base URL   (default: https://ntfy.sh)
#   TEST_NTFY_TOPIC    topic           (default: agent)
#   TEST_NTFY_MESSAGE  message         (default: test message)
#   MCP_BASE_URL       MCP endpoint    (default: http://127.0.0.1:12120/mcp)
set -euo pipefail

cd "$(dirname "$0")"
# shellcheck source=lib.sh
source ./lib.sh

TEST_NTFY_SERVER="${TEST_NTFY_SERVER:-https://ntfy.sh}"
TEST_NTFY_TOPIC="${TEST_NTFY_TOPIC:-agent}"
TEST_NTFY_MESSAGE="${TEST_NTFY_MESSAGE:-test message}"
: "${TEST_NTFY_TOKEN:?set TEST_NTFY_TOKEN to a real ntfy access token}"

SESSION="$(mcp_initialize)"
[ -n "$SESSION" ] || { echo "ERROR: no MCP session id" >&2; exit 1; }
echo "Session : $SESSION"

mcp_notify_initialized "$SESSION"

mcp_tool_call "$SESSION" 2 send_notification "{\"base_url\":\"$TEST_NTFY_SERVER\",\"message\":\"$TEST_NTFY_MESSAGE\",\"title\":\"MCP test\",\"topic\":\"$TEST_NTFY_TOPIC\",\"token\":\"$TEST_NTFY_TOKEN\",\"priority\":3}" \
    | jq -c '.result // .error'
