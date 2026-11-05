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
# configurable via FAKE_NTFY_PORT / MCP_TEST_PORT / MCP_AUTH_TEST_PORT,
# no interaction needed.
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
MCP_AUTH_PORT="${MCP_AUTH_TEST_PORT:-12322}"
MCP_GUARD_PORT="${MCP_GUARD_TEST_PORT:-12323}"
MCP_PROXY_PORT="${MCP_PROXY_TEST_PORT:-12324}"
MCP_SHUTDOWN_PORT="${MCP_SHUTDOWN_TEST_PORT:-12325}"
FAKE_NTFY_URL="http://127.0.0.1:$FAKE_NTFY_PORT"
export MCP_BASE_URL="http://127.0.0.1:$MCP_TEST_PORT/mcp"
TEST_TOKEN="test-secret-token"
TEST_TOPIC="testtopic"

TMP="$(mktemp -d)"
FAKE_PID=""
MCP_PID=""
MCP_AUTH_PID=""
MCP_GUARD_PID=""
MCP_PROXY_PID=""
MCP_SHUTDOWN_PID=""
# Servers are started with setsid so cleanup can kill the whole process
# group (e.g. "go run" and the compiled fake-ntfy it spawns).
cleanup() {
    [ -n "$MCP_PID" ] && kill -- -"$MCP_PID" 2>/dev/null || true
    [ -n "$MCP_AUTH_PID" ] && kill -- -"$MCP_AUTH_PID" 2>/dev/null || true
    [ -n "$MCP_GUARD_PID" ] && kill -- -"$MCP_GUARD_PID" 2>/dev/null || true
    [ -n "$MCP_PROXY_PID" ] && kill -- -"$MCP_PROXY_PID" 2>/dev/null || true
    [ -n "$MCP_SHUTDOWN_PID" ] && kill -- -"$MCP_SHUTDOWN_PID" 2>/dev/null || true
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
if port_in_use "$FAKE_NTFY_PORT" || port_in_use "$MCP_TEST_PORT" || port_in_use "$MCP_AUTH_PORT" || port_in_use "$MCP_GUARD_PORT" || port_in_use "$MCP_PROXY_PORT" || port_in_use "$MCP_SHUTDOWN_PORT"; then
    echo "ERROR: port $FAKE_NTFY_PORT, $MCP_TEST_PORT, $MCP_AUTH_PORT, $MCP_GUARD_PORT, $MCP_PROXY_PORT or $MCP_SHUTDOWN_PORT is already in use;" >&2
    echo "set FAKE_NTFY_PORT / MCP_TEST_PORT / MCP_AUTH_TEST_PORT / MCP_GUARD_TEST_PORT / MCP_PROXY_TEST_PORT / MCP_SHUTDOWN_TEST_PORT to free ports" >&2
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
# Short guard settings so phase 3 can exercise the anti-DoS block quickly:
# 3 upstream 401s within 60s block the target for 2s.
setsid env NTFY_URL="$FAKE_NTFY_URL" NTFY_TOPIC="$TEST_TOPIC" NTFY_TOKEN="$TEST_TOKEN" \
    MCP_NTFY_PORT="$MCP_TEST_PORT" \
    MCP_NTFY_RATE_LIMIT_FAILURES=3 MCP_NTFY_RATE_LIMIT_WINDOW=60 MCP_NTFY_RATE_LIMIT_BLOCK=2 \
    MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1 \
    "$TMP/ntfy-mcp" >"$TMP/mcp.log" 2>&1 &
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

# 7b. Extended ntfy fields (click, icon, delay, email, markdown) are forwarded
# upstream and come back on read. Own topic so the default-topic reads below
# stay unaffected.
R="$(mcp_tool_call "$SESSION" "$(next_id)" send_notification '{"message":"rich","topic":"fieldstopic","title":"Rich","click":"https://example.com/docs","icon":"https://example.com/icon.png","delay":"30min","email":"ops@example.com","markdown":true}')"
assert_jq "send with extended fields succeeds" "$R" '.result.content[0].text' "Notification sent successfully."
R="$(mcp_tool_call "$SESSION" "$(next_id)" read_notifications '{"topic":"fieldstopic"}')"
T="$(printf '%s' "$R" | jq -r '.result.content[0].text')"
assert_contains "click round-trips" "$T" "https://example.com/docs"
assert_contains "icon round-trips" "$T" "https://example.com/icon.png"
assert_contains "delay round-trips" "$T" "30min"
assert_contains "email round-trips" "$T" "ops@example.com"
assert_contains "markdown round-trips" "$T" '"markdown": true'

# 7c. Extended-field validation rejects bad input before any upstream call
# (so the anti-DoS rejection counts in phase 3 are unaffected).
R="$(mcp_tool_call "$SESSION" "$(next_id)" send_notification '{"message":"x","click":"javascript:alert(1)"}')"
assert_jq "javascript: click rejected" "$R" '.result.isError' "true"
assert_contains "click scheme error is explicit" "$R" "http(s)"
R="$(mcp_tool_call "$SESSION" "$(next_id)" send_notification '{"message":"x","icon":"ftp://example.com/i.png"}')"
assert_jq "non-http(s) icon rejected" "$R" '.result.isError' "true"
R="$(mcp_tool_call "$SESSION" "$(next_id)" send_notification '{"message":"x","delay":"30min\nX-Evil: 1"}')"
assert_jq "delay with control chars rejected" "$R" '.result.isError' "true"
assert_contains "delay error is explicit" "$R" "invalid delay"
R="$(mcp_tool_call "$SESSION" "$(next_id)" send_notification '{"message":"x","email":"not-an-email"}')"
assert_jq "invalid email rejected" "$R" '.result.isError' "true"
assert_contains "email error is explicit" "$R" "invalid email"
BIGCLICK="https://example.com/$(printf 'a%.0s' $(seq 1 2100))"
R="$(mcp_tool_call "$SESSION" "$(next_id)" send_notification "{\"message\":\"x\",\"click\":\"$BIGCLICK\"}")"
assert_jq "oversized click rejected" "$R" '.result.isError' "true"
assert_contains "click size error mentions the limit" "$R" "2048"

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

# 11b. Upstream error bodies are not leaked to the MCP client: the fake
# ntfy 401 body ({"error":"unauthorized"}) must not appear in the tool
# result, only the generic failure message. (This is a 2nd upstream 401
# on the main instance's guard; phase 3's counts below account for the
# one from test 11 only, because the guard blocks after 3 rejections.)
R="$(mcp_tool_call "$SESSION" "$(next_id)" read_notifications "{\"base_url\":\"$FAKE_NTFY_URL\",\"token\":\"wrong-token\"}")"
assert_jq "upstream rejection surfaces as tool error" "$R" '.result.isError' "true"
assert_not_contains "upstream body not leaked to client" "$R" '"error":"unauthorized"'
assert_contains "client gets a generic upstream error" "$R" "failed to read notifications"

# 12. The token never appears in cleartext in the server logs.
if grep -qF -- "$TEST_TOKEN" "$TMP/mcp.log"; then
    ko "token absent from logs"
else
    ok "token absent from logs"
fi
assert_contains "logs show token as redacted" "$(cat "$TMP/mcp.log")" "[redacted]"

echo "== phase 2: MCP client authentication (MCP_NTFY_CLIENT_TOKEN) =="
AUTH_URL="http://127.0.0.1:$MCP_AUTH_PORT/mcp"
CLIENT_TOKEN="mcp-client-test-secret"
setsid env NTFY_URL="$FAKE_NTFY_URL" NTFY_TOPIC="$TEST_TOPIC" NTFY_TOKEN="$TEST_TOKEN" \
    MCP_NTFY_PORT="$MCP_AUTH_PORT" MCP_NTFY_CLIENT_TOKEN="$CLIENT_TOKEN" \
    MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1 \
    "$TMP/ntfy-mcp" >"$TMP/mcp-auth.log" 2>&1 &
MCP_AUTH_PID=$!
wait_for_port 127.0.0.1 "$MCP_AUTH_PORT"

INIT_BODY='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}'

# 13. Without a token, the request is rejected with 401 before reaching MCP.
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST "$AUTH_URL" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -d "$INIT_BODY")"
if [ "$code" = "401" ]; then ok "no client token -> HTTP 401"; else ko "no client token -> HTTP 401 (got $code)"; fi

# 14. A wrong token is also rejected with 401.
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST "$AUTH_URL" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -H 'Authorization: Bearer wrong-token' \
    -d "$INIT_BODY")"
if [ "$code" = "401" ]; then ok "wrong client token -> HTTP 401"; else ko "wrong client token -> HTTP 401 (got $code)"; fi

# 14b. Repeated failed authentications from one IP are throttled with 429
# (MCP_NTFY_AUTH_FAILURE_LIMIT defaults to 10 per window; 2 failures so
# far, so the 9th extra attempt already exceeds the limit).
CODES=""
for i in $(seq 1 10); do
    c="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST "$AUTH_URL" \
        -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
        -H 'Authorization: Bearer wrong-token' \
        -d "$INIT_BODY")"
    CODES="$CODES $c"
done
if printf '%s' "$CODES" | grep -q '429'; then
    ok "repeated auth failures throttled with HTTP 429 ($CODES)"
else
    ko "repeated auth failures throttled with HTTP 429 (got:$CODES)"
fi

# 15. With the correct token, the full MCP flow works.
MCP_BASE_URL="$AUTH_URL"
MCP_AUTH_TOKEN="$CLIENT_TOKEN"
MCP_AUTH_ARGS=(-H "Authorization: Bearer $MCP_AUTH_TOKEN")
AUTH_SESSION="$(mcp_initialize)"
if [ -n "$AUTH_SESSION" ]; then ok "initialize with valid client token succeeds"; else ko "initialize with valid client token succeeds"; fi
mcp_notify_initialized "$AUTH_SESSION"
TOOLS="$(mcp_call "$AUTH_SESSION" "$(next_id)" tools/list '{}')"
assert_contains "authenticated tools/list exposes send_notification" \
    "$(printf '%s' "$TOOLS" | jq -r '.result.tools[].name' 2>/dev/null || true)" "send_notification"
R="$(mcp_tool_call "$AUTH_SESSION" "$(next_id)" send_notification '{"message":"via authenticated client"}')"
assert_jq "authenticated send_notification succeeds" "$R" '.result.content[0].text' "Notification sent successfully."

# 16. The notification sent through the authenticated instance reached ntfy
# (readable from the first, unauthenticated instance: same backend topic).
R="$(mcp_tool_call "$SESSION" "$(next_id)" read_notifications '{}')"
assert_contains "authenticated notification reached the ntfy topic" \
    "$(printf '%s' "$R" | jq -r '.result.content[0].text')" "via authenticated client"

# 17. The client token never appears in the server logs.
if grep -qF -- "$CLIENT_TOKEN" "$TMP/mcp-auth.log"; then
    ko "client token absent from auth-server logs"
else
    ok "client token absent from auth-server logs"
fi
assert_contains "auth server logs show client auth enabled" "$(cat "$TMP/mcp-auth.log")" "MCP client authentication enabled"

# Restore the unauthenticated endpoint for any later use.
MCP_BASE_URL="http://127.0.0.1:$MCP_TEST_PORT/mcp"
MCP_AUTH_TOKEN=""
MCP_AUTH_ARGS=()

echo "== phase 3: upstream anti-DoS guard (block target after repeated 401s) =="
# Tests 11 and 11b above already caused two upstream 401s on
# $FAKE_NTFY_URL; with MCP_NTFY_RATE_LIMIT_FAILURES=3, one more
# rejection triggers the block.
R="$(mcp_tool_call "$SESSION" "$(next_id)" read_notifications "{\"base_url\":\"$FAKE_NTFY_URL\",\"token\":\"wrong-token\"}")"
assert_jq "3rd upstream 401 surfaces as tool error (triggers block)" "$R" '.result.isError' "true"

# The target is now blocked: further requests are refused locally...
R="$(mcp_tool_call "$SESSION" "$(next_id)" read_notifications "{\"base_url\":\"$FAKE_NTFY_URL\",\"token\":\"wrong-token\"}")"
assert_jq "4th request blocked locally" "$R" '.result.isError' "true"
assert_contains "block message explains why" "$R" "temporarily blocked"
# ...even with a valid token, because the guard is keyed by target URL
# (rotating tokens must not evade the block).
R="$(mcp_tool_call "$SESSION" "$(next_id)" read_notifications "{\"base_url\":\"$FAKE_NTFY_URL\",\"token\":\"$TEST_TOKEN\"}")"
assert_contains "valid token also blocked for that target" "$R" "temporarily blocked"

# The block is keyed on the canonical target: a trailing-slash variant of
# the same URL must not evade it.
R="$(mcp_tool_call "$SESSION" "$(next_id)" read_notifications "{\"base_url\":\"$FAKE_NTFY_URL/\",\"token\":\"$TEST_TOKEN\"}")"
assert_contains "trailing-slash variant also blocked" "$R" "temporarily blocked"

# ...and nothing more was forwarded upstream: the fake ntfy saw exactly
# the 3 rejected requests (tests 11, 11b and the first phase-3 call).
N401="$(grep -c 'HTTP 401' "$TMP/fake.log" || true)"
if [ "$N401" = "3" ]; then
    ok "blocked requests never forwarded (fake ntfy saw exactly 3 x 401)"
else
    ko "blocked requests never forwarded (fake ntfy saw $N401 x 401, want 3)"
fi
assert_contains "mcp log records the block" "$(cat "$TMP/mcp.log")" "BLOCKED"

# After the block expires (MCP_NTFY_RATE_LIMIT_BLOCK=2s), the target works again.
sleep 3
R="$(mcp_tool_call "$SESSION" "$(next_id)" read_notifications "{\"base_url\":\"$FAKE_NTFY_URL\",\"token\":\"$TEST_TOKEN\"}")"
assert_contains "target usable again after block expiry" "$(printf '%s' "$R" | jq -r '.result.content[0].text')" "hello one"

echo "== phase 4: stdio transport (MCP_NTFY_TRANSPORT=stdio) =="
# Newline-delimited JSON-RPC on stdin; the server exits cleanly at EOF.
# stdin is held open briefly after the last message: mcp-go cancels the
# context of in-flight requests when it sees EOF, which would abort the
# send_notification HTTP call before it reaches the fake ntfy server.
# The stdio server dispatches requests concurrently, so the read is sent
# only after a pause: otherwise its GET could reach the fake ntfy before
# the send's POST has been published.
STDIO_OUT="$({ printf '%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"stdio-test","version":"1.0"}}}' \
    '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send_notification","arguments":{"message":"hello via stdio","title":"Stdio"}}}'; \
    sleep 2; \
    printf '%s\n' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read_notifications","arguments":{}}}'; \
    sleep 3; } | env NTFY_URL="$FAKE_NTFY_URL" NTFY_TOPIC="$TEST_TOPIC" NTFY_TOKEN="$TEST_TOKEN" \
        MCP_NTFY_TRANSPORT=stdio MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1 "$TMP/ntfy-mcp" 2>"$TMP/mcp-stdio.log")"

assert_jq "stdio initialize handshake" "$STDIO_OUT" 'select(.id==1) | .result.serverInfo.name' "ntfy-mcp"
STDIO_TOOLS="$(printf '%s' "$STDIO_OUT" | jq -r 'select(.id==2) | .result.tools[].name' 2>/dev/null || true)"
assert_contains "stdio tools/list exposes send_notification" "$STDIO_TOOLS" "send_notification"
assert_contains "stdio tools/list exposes read_notifications" "$STDIO_TOOLS" "read_notifications"
assert_jq "stdio send_notification succeeds" "$STDIO_OUT" 'select(.id==3) | .result.content[0].text' "Notification sent successfully."
STDIO_READ="$(printf '%s' "$STDIO_OUT" | jq -r 'select(.id==4) | .result.content[0].text' 2>/dev/null || true)"
assert_contains "stdio read_notifications sees the stdio-sent message" "$STDIO_READ" "hello via stdio"
assert_not_contains "logs stay off the stdio protocol channel" "$STDIO_OUT" "[ntfy-mcp]"
assert_contains "stdio mode logged on stderr" "$(cat "$TMP/mcp-stdio.log")" "serving MCP over stdio"

# An invalid transport value is rejected at startup.
if env MCP_NTFY_TRANSPORT=bogus "$TMP/ntfy-mcp" >/dev/null 2>"$TMP/mcp-bogus.log"; then
    ko "invalid MCP_NTFY_TRANSPORT rejected at startup"
else
    ok "invalid MCP_NTFY_TRANSPORT rejected at startup"
fi
assert_contains "invalid transport error is explicit" "$(cat "$TMP/mcp-bogus.log")" "not a valid transport"

echo "== phase 5: input validation and SSRF guard (no MCP_NTFY_ALLOW_PRIVATE_UPSTREAM) =="
# This instance runs with the secure defaults: private/loopback upstream
# IPs are refused at dial time, base_url must be http(s), topic must
# match the ntfy charset, message size is capped.
setsid env NTFY_URL="$FAKE_NTFY_URL" NTFY_TOPIC="$TEST_TOPIC" NTFY_TOKEN="$TEST_TOKEN" \
    MCP_NTFY_PORT="$MCP_GUARD_PORT" \
    "$TMP/ntfy-mcp" >"$TMP/mcp-guard.log" 2>&1 &
MCP_GUARD_PID=$!
wait_for_port 127.0.0.1 "$MCP_GUARD_PORT"

GUARD_URL="http://127.0.0.1:$MCP_GUARD_PORT/mcp"
MCP_BASE_URL="$GUARD_URL"
MCP_AUTH_TOKEN=""
MCP_AUTH_ARGS=()
GSESSION="$(mcp_initialize)"
[ -n "$GSESSION" ] || { echo "FATAL: could not initialize guard-instance MCP session" >&2; exit 1; }
mcp_notify_initialized "$GSESSION"

# 18. Dial-time SSRF guard: a loopback target is refused even with a
# request token (MCP_NTFY_ALLOW_PRIVATE_UPSTREAM is not set here).
R="$(mcp_tool_call "$GSESSION" "$(next_id)" send_notification "{\"base_url\":\"$FAKE_NTFY_URL\",\"token\":\"$TEST_TOKEN\",\"message\":\"ssrf attempt\"}")"
assert_jq "loopback upstream refused" "$R" '.result.isError' "true"
assert_contains "refusal explains the IP block" "$R" "not allowed"

# 19. Non-http(s) schemes are rejected.
R="$(mcp_tool_call "$GSESSION" "$(next_id)" send_notification '{"base_url":"ftp://example.com","token":"x","message":"x"}')"
assert_jq "ftp base_url rejected" "$R" '.result.isError' "true"
assert_contains "scheme error mentions http/https" "$R" "http or https"

# 20. Topic charset is validated: no path traversal / query injection.
R="$(mcp_tool_call "$GSESSION" "$(next_id)" send_notification '{"topic":"../other?x=1","message":"x"}')"
assert_jq "malicious topic rejected" "$R" '.result.isError' "true"
assert_contains "topic error is explicit" "$R" "invalid topic"

# 21. Message size is capped at the ntfy limit (4096 bytes).
BIG="$(printf 'a%.0s' $(seq 1 5000))"
R="$(mcp_tool_call "$GSESSION" "$(next_id)" send_notification "{\"message\":\"$BIG\"}")"
assert_jq "oversized message rejected" "$R" '.result.isError' "true"
assert_contains "size error mentions the limit" "$R" "4096"

# 22. NAT64 well-known prefix (64:ff9b::/96) is refused at dial time.
R="$(mcp_tool_call "$GSESSION" "$(next_id)" send_notification "{\"base_url\":\"http://[64:ff9b::1]/\",\"token\":\"x\",\"message\":\"x\"}")"
assert_jq "NAT64 prefix refused" "$R" '.result.isError' "true"
assert_contains "NAT64 refusal explains the IP block" "$R" "not allowed"

# 23. Title and tags are size-capped before any upstream call.
BIGTITLE="$(printf 't%.0s' $(seq 1 600))"
R="$(mcp_tool_call "$GSESSION" "$(next_id)" send_notification "{\"title\":\"$BIGTITLE\",\"message\":\"x\"}")"
assert_jq "oversized title rejected" "$R" '.result.isError' "true"
assert_contains "title error mentions the limit" "$R" "512"
BIGTAGS="$(printf 'g%.0s' $(seq 1 600))"
R="$(mcp_tool_call "$GSESSION" "$(next_id)" send_notification "{\"tags\":\"$BIGTAGS\",\"message\":\"x\"}")"
assert_jq "oversized tags rejected" "$R" '.result.isError' "true"
assert_contains "tags error mentions the limit" "$R" "512"

# Restore the default endpoint.
MCP_BASE_URL="http://127.0.0.1:$MCP_TEST_PORT/mcp"

echo "== phase 6: trusted proxy (MCP_NTFY_TRUSTED_PROXIES) =="
# This instance trusts X-Forwarded-For from loopback peers and has a low
# client rate limit (5 per 60s) so the per-IP buckets are observable.
setsid env NTFY_URL="$FAKE_NTFY_URL" NTFY_TOPIC="$TEST_TOPIC" NTFY_TOKEN="$TEST_TOKEN" \
    MCP_NTFY_PORT="$MCP_PROXY_PORT" \
    MCP_NTFY_CLIENT_RATE_LIMIT=5 MCP_NTFY_RATE_LIMIT_WINDOW=60 \
    MCP_NTFY_TRUSTED_PROXIES="127.0.0.0/8" \
    MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1 \
    "$TMP/ntfy-mcp" >"$TMP/mcp-proxy.log" 2>&1 &
MCP_PROXY_PID=$!
wait_for_port 127.0.0.1 "$MCP_PROXY_PORT"

PROXY_URL="http://127.0.0.1:$MCP_PROXY_PORT/mcp"
proxy_post() { # [XFF] -> http status code
    if [ -n "${1:-}" ]; then
        curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST "$PROXY_URL" \
            -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
            -H "X-Forwarded-For: $1" -d "$INIT_BODY"
    else
        curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST "$PROXY_URL" \
            -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
            -d "$INIT_BODY"
    fi
}

# Same forwarded IP: the first 5 requests pass, the 6th is throttled.
CODES=""
for i in $(seq 1 6); do CODES="$CODES $(proxy_post 10.9.9.9)"; done
if printf '%s' "$CODES" | grep -q '429'; then
    ok "same X-Forwarded-For IP throttled after 5 requests ($CODES)"
else
    ko "same X-Forwarded-For IP throttled after 5 requests (got:$CODES)"
fi

# Distinct forwarded IPs: each gets its own bucket, no 429.
CODES=""
for i in $(seq 1 8); do CODES="$CODES $(proxy_post "10.8.8.$i")"; done
if printf '%s' "$CODES" | grep -q '429'; then
    ko "distinct X-Forwarded-For IPs not throttled (got:$CODES)"
else
    ok "distinct X-Forwarded-For IPs not throttled ($CODES)"
fi

# Without X-Forwarded-For the direct peer IP (127.0.0.1) is the key.
CODES=""
for i in $(seq 1 6); do CODES="$CODES $(proxy_post)"; done
if printf '%s' "$CODES" | grep -q '429'; then
    ok "no X-Forwarded-For falls back to the peer IP and is throttled ($CODES)"
else
    ko "no X-Forwarded-For falls back to the peer IP and is throttled (got:$CODES)"
fi

echo "== phase 7: /healthz and graceful shutdown (SIGTERM/SIGINT) =="

# 24. /healthz answers 200 "ok" on the plain instance (no client token set).
code="$(curl -s -o "$TMP/healthz.body" -w '%{http_code}' --max-time 5 "http://127.0.0.1:$MCP_TEST_PORT/healthz")"
if [ "$code" = "200" ]; then ok "/healthz returns HTTP 200"; else ko "/healthz returns HTTP 200 (got $code)"; fi
assert_contains "/healthz body is ok" "$(cat "$TMP/healthz.body")" "ok"

# 25. Only GET/HEAD are allowed on /healthz.
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST "http://127.0.0.1:$MCP_TEST_PORT/healthz" \
    -H 'Content-Type: application/json' -d '{}')"
if [ "$code" = "405" ]; then ok "POST /healthz -> HTTP 405"; else ko "POST /healthz -> HTTP 405 (got $code)"; fi

# 26-28. Dedicated instance for the auth + signal tests on a fresh port.
start_shutdown_instance() { # $1 = extra env assignment (may be empty)
    setsid env NTFY_URL="$FAKE_NTFY_URL" NTFY_TOPIC="$TEST_TOPIC" NTFY_TOKEN="$TEST_TOKEN" \
        MCP_NTFY_PORT="$MCP_SHUTDOWN_PORT" MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1 $1 \
        "$TMP/ntfy-mcp" >"$TMP/mcp-shutdown.log" 2>&1 &
    MCP_SHUTDOWN_PID=$!
    wait_for_port 127.0.0.1 "$MCP_SHUTDOWN_PORT"
}
stop_with_signal() { # $1 = signal name; sets RC to the server exit code
    kill "-$1" "$MCP_SHUTDOWN_PID"
    ( sleep 10; kill -9 "$MCP_SHUTDOWN_PID" 2>/dev/null ) & # bound the wait
    local killer=$!
    RC=0
    wait "$MCP_SHUTDOWN_PID" || RC=$?
    kill "$killer" 2>/dev/null || true
}

# With MCP_NTFY_CLIENT_TOKEN set, /healthz requires the Bearer token like
# every other endpoint.
start_shutdown_instance "MCP_NTFY_CLIENT_TOKEN=$CLIENT_TOKEN"
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:$MCP_SHUTDOWN_PORT/healthz")"
if [ "$code" = "401" ]; then ok "/healthz without client token -> HTTP 401"; else ko "/healthz without client token -> HTTP 401 (got $code)"; fi
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H "Authorization: Bearer $CLIENT_TOKEN" "http://127.0.0.1:$MCP_SHUTDOWN_PORT/healthz")"
if [ "$code" = "200" ]; then ok "/healthz with client token -> HTTP 200"; else ko "/healthz with client token -> HTTP 200 (got $code)"; fi

# SIGTERM: graceful exit 0, shutdown logged, listener closed.
stop_with_signal TERM
if [ "$RC" = "0" ]; then ok "SIGTERM -> graceful exit code 0"; else ko "SIGTERM -> graceful exit code 0 (got $RC)"; fi
assert_contains "SIGTERM shutdown logged" "$(cat "$TMP/mcp-shutdown.log")" "shutdown signal received"
assert_contains "drain completes before exit" "$(cat "$TMP/mcp-shutdown.log")" "graceful shutdown complete"
if port_in_use "$MCP_SHUTDOWN_PORT"; then ko "listener closed after SIGTERM"; else ok "listener closed after SIGTERM"; fi

# SIGINT behaves the same.
start_shutdown_instance ""
stop_with_signal INT
if [ "$RC" = "0" ]; then ok "SIGINT -> graceful exit code 0"; else ko "SIGINT -> graceful exit code 0 (got $RC)"; fi

echo
echo "== results: $PASS passed, $FAIL failed =="
if [ "$FAIL" -gt 0 ]; then
    # Stop the test processes but keep the logs around for debugging.
    kill -- -"$MCP_PID" -"$MCP_AUTH_PID" -"$MCP_GUARD_PID" -"$MCP_PROXY_PID" -"$FAKE_PID" 2>/dev/null || true
    trap - EXIT
    echo "Test logs kept at: $TMP (mcp.log, fake.log)" >&2
    exit 1
fi
exit 0
