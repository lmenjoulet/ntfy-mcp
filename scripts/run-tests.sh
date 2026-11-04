#!/usr/bin/env bash
# Hermetic end-to-end test suite for ntfy-mcp.
#
# Builds the server from current sources, starts a fake ntfy server
# (scripts/fake-ntfy.go) and the MCP server locally, then exercises the
# exposed tools over the MCP protocol. No real ntfy server, token or
# external network access is required.
#
# Exit code 0 = all tests passed, 1 = at least one failure.
# Designed to run unchanged in CI (GitHub Actions, etc.): ports are
# configurable via FAKE_NTFY_PORT / MCP_TEST_PORT, no interaction needed.
#
# Usage:
#   nix develop --command bash scripts/run-tests.sh
#   # or with Go directly on PATH:
#   bash scripts/run-tests.sh
set -euo pipefail

cd "$(dirname "$0")/.."
SCRIPT_DIR="$PWD/scripts"

FAKE_NTFY_PORT="${FAKE_NTFY_PORT:-18099}"
MCP_TEST_PORT="${MCP_TEST_PORT:-12321}"
FAKE_NTFY_URL="http://127.0.0.1:$FAKE_NTFY_PORT"
export MCP_BASE_URL="http://127.0.0.1:$MCP_TEST_PORT/mcp"
TEST_TOKEN="test-secret-token"
TEST_TOPIC="testtopic"

TMP="$(mktemp -d)"
FAKE_PID=""
MCP_PID=""
# Servers are started with setsid so cleanup can kill the whole process
# group (e.g. "go run" and the compiled fake-ntfy it spawns).
cleanup() {
    [ -n "$MCP_PID" ] && kill -- -"$MCP_PID" 2>/dev/null || true
    [ -n "$FAKE_PID" ] && kill -- -"$FAKE_PID" 2>/dev/null || true
    rm -rf "$TMP"
}
trap cleanup EXIT

port_in_use() {
    if (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; then
        exec 3>&- 3<&-
        return 0
    fi
    return 1
}
if port_in_use "$FAKE_NTFY_PORT" || port_in_use "$MCP_TEST_PORT"; then
    echo "ERROR: port $FAKE_NTFY_PORT or $MCP_TEST_PORT is already in use;" >&2
    echo "set FAKE_NTFY_PORT / MCP_TEST_PORT to free ports" >&2
    exit 1
fi

# shellcheck source=lib.sh
source "$SCRIPT_DIR/lib.sh"

PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); echo "PASS: $1"; }
ko() { FAIL=$((FAIL + 1)); echo "FAIL: $1"; }

# assert_contains NAME HAYSTACK NEEDLE
assert_contains() {
    if printf '%s' "$2" | grep -qF -- "$3"; then ok "$1"; else ko "$1 -- expected to contain: $3"; fi
}
assert_not_contains() {
    if printf '%s' "$2" | grep -qF -- "$3"; then ko "$1 -- expected NOT to contain: $3"; else ok "$1"; fi
}
# assert_jq NAME JSON JQ_EXPR EXPECTED  (jq -r output compared as string)
assert_jq() {
    local got
    got=$(printf '%s' "$2" | jq -r "$3" 2>/dev/null || echo "__JQ_ERROR__")
    if [ "$got" = "$4" ]; then ok "$1"; else ko "$1 -- got: $got, want: $4"; fi
}

echo "== building ntfy-mcp =="
go build -o "$TMP/ntfy-mcp" .

echo "== starting fake ntfy on $FAKE_NTFY_URL =="
setsid env EXPECT_TOKEN="$TEST_TOKEN" go run "$SCRIPT_DIR/fake-ntfy.go" -addr "127.0.0.1:$FAKE_NTFY_PORT" >"$TMP/fake.log" 2>&1 &
FAKE_PID=$!

echo "== starting ntfy-mcp on $MCP_BASE_URL =="
setsid env NTFY_URL="$FAKE_NTFY_URL" NTFY_TOPIC="$TEST_TOPIC" NTFY_TOKEN="$TEST_TOKEN" \
    MCP_NTFY_PORT="$MCP_TEST_PORT" "$TMP/ntfy-mcp" >"$TMP/mcp.log" 2>&1 &
MCP_PID=$!

wait_for_port 127.0.0.1 "$FAKE_NTFY_PORT"
wait_for_port 127.0.0.1 "$MCP_TEST_PORT"

SESSION="$(mcp_initialize)"
if [ -z "$SESSION" ]; then
    echo "FATAL: could not initialize MCP session" >&2
    exit 1
fi
mcp_notify_initialized "$SESSION"

ID=100
next_id() { ID=$((ID + 1)); echo "$ID"; }

echo "== running tests =="

# 1. Both tools are exposed.
TOOLS="$(mcp_call "$SESSION" "$(next_id)" tools/list '{}')"
NAMES="$(printf '%s' "$TOOLS" | jq -r '.result.tools[].name' 2>/dev/null || true)"
assert_contains "tools/list exposes send_notification" "$NAMES" "send_notification"
assert_contains "tools/list exposes read_notifications" "$NAMES" "read_notifications"

# 2-3. Send two notifications.
R="$(mcp_tool_call "$SESSION" "$(next_id)" send_notification '{"message":"hello one","title":"First"}')"
assert_jq "send_notification succeeds" "$R" '.result.content[0].text' "Notification sent successfully."
R="$(mcp_tool_call "$SESSION" "$(next_id)" send_notification '{"message":"hello two","title":"Second","priority":5,"tags":"warning,phone"}')"
assert_jq "second send_notification succeeds" "$R" '.result.content[0].text' "Notification sent successfully."

# 4. read_notifications returns both messages.
R="$(mcp_tool_call "$SESSION" "$(next_id)" read_notifications '{}')"
T="$(printf '%s' "$R" | jq -r '.result.content[0].text')"
assert_contains "read returns first message" "$T" "hello one"
assert_contains "read returns second message" "$T" "hello two"
assert_contains "read preserves title" "$T" "Second"
assert_contains "read preserves tags" "$T" "warning"

# 5. limit truncates client-side.
R="$(mcp_tool_call "$SESSION" "$(next_id)" read_notifications '{"limit":1}')"
T="$(printf '%s' "$R" | jq -r '.result.content[0].text')"
assert_contains "limit=1 keeps first message" "$T" "hello one"
assert_not_contains "limit=1 drops second message" "$T" "hello two"

# 6. since=<id> returns only later messages.
R="$(mcp_tool_call "$SESSION" "$(next_id)" read_notifications '{"since":"id-1"}')"
T="$(printf '%s' "$R" | jq -r '.result.content[0].text')"
assert_not_contains "since=id-1 excludes first message" "$T" "hello one"
assert_contains "since=id-1 keeps second message" "$T" "hello two"

# 7. Unknown topic yields an explicit empty result.
R="$(mcp_tool_call "$SESSION" "$(next_id)" read_notifications '{"topic":"no-such-topic"}')"
assert_jq "empty topic reported" "$R" '.result.content[0].text' "No notifications found."

# 8. Empty message is rejected (protocol-level error, not a tool error).
R="$(mcp_tool_call "$SESSION" "$(next_id)" send_notification '{"message":"   "}')"
assert_contains "empty message rejected" "$R" "message parameter cannot be empty"

# 9. Anti-exfiltration guard: custom base_url without a request token is rejected.
R="$(mcp_tool_call "$SESSION" "$(next_id)" read_notifications '{"base_url":"https://evil.example.com"}')"
assert_jq "custom base_url without token rejected" "$R" '.result.isError' "true"
assert_contains "rejection explains why" "$R" "custom base_url requires a token in the request"
R="$(mcp_tool_call "$SESSION" "$(next_id)" send_notification '{"base_url":"https://evil.example.com","message":"x"}')"
assert_jq "send to custom base_url without token rejected" "$R" '.result.isError' "true"

# 10. Custom base_url WITH a request token works (public-server regime).
R="$(mcp_tool_call "$SESSION" "$(next_id)" read_notifications "{\"base_url\":\"$FAKE_NTFY_URL\",\"token\":\"$TEST_TOKEN\"}")"
assert_contains "read with request token works" "$(printf '%s' "$R" | jq -r '.result.content[0].text')" "hello one"

# 11. Wrong token is rejected by the ntfy server (proves the token is forwarded).
R="$(mcp_tool_call "$SESSION" "$(next_id)" read_notifications "{\"base_url\":\"$FAKE_NTFY_URL\",\"token\":\"wrong-token\"}")"
assert_jq "wrong token rejected" "$R" '.result.isError' "true"

# 12. The token never appears in cleartext in the server logs.
if grep -qF -- "$TEST_TOKEN" "$TMP/mcp.log"; then
    ko "token absent from logs"
else
    ok "token absent from logs"
fi
assert_contains "logs show token as redacted" "$(cat "$TMP/mcp.log")" "[redacted]"

echo
echo "== results: $PASS passed, $FAIL failed =="
if [ "$FAIL" -gt 0 ]; then
    # Stop the test processes but keep the logs around for debugging.
    kill -- -"$MCP_PID" -"$FAKE_PID" 2>/dev/null || true
    trap - EXIT
    echo "Test logs kept at: $TMP (mcp.log, fake.log)" >&2
    exit 1
fi
exit 0
