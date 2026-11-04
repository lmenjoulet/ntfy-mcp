# Common helpers for ntfy-mcp test scripts. Source this file, do not run it.
#
# Variables:
#   MCP_BASE_URL  MCP endpoint (default: http://127.0.0.1:12321/mcp)

MCP_BASE_URL="${MCP_BASE_URL:-http://127.0.0.1:12321/mcp}"

# wait_for_http URL [TRIES] — succeed once URL answers any HTTP response.
wait_for_http() {
    local url="$1" tries="${2:-50}" i
    for i in $(seq "$tries"); do
        if curl -s --max-time 2 -o /dev/null "$url"; then
            return 0
        fi
        sleep 0.1
    done
    echo "ERROR: $url did not come up after $tries tries" >&2
    return 1
}

# wait_for_port HOST PORT [TRIES] — succeed once the TCP port accepts
# connections. Prefer this for the MCP server: a bare GET on /mcp opens
# an SSE stream and can hang for seconds.
wait_for_port() {
    local host="$1" port="$2" tries="${3:-50}" i
    for i in $(seq "$tries"); do
        if (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null; then
            exec 3>&- 3<&-
            return 0
        fi
        sleep 0.1
    done
    echo "ERROR: $host:$port did not come up after $tries tries" >&2
    return 1
}

# mcp_initialize — perform the MCP initialize handshake, echoes the session id.
mcp_initialize() {
    local headers
    headers=$(mktemp)
    curl -s --max-time 10 -D "$headers" -o /dev/null -X POST "$MCP_BASE_URL" \
        -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}'
    grep -i '^mcp-session-id:' "$headers" | tr -d '\r' | awk '{print $2}'
    rm -f "$headers"
}

# mcp_notify_initialized SESSION — send the initialized notification.
mcp_notify_initialized() {
    curl -s --max-time 5 -o /dev/null -X POST "$MCP_BASE_URL" \
        -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        -H "Mcp-Session-Id: $1" \
        -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
}

# mcp_call SESSION ID METHOD PARAMS_JSON — one JSON-RPC call, echoes the response body.
mcp_call() {
    curl -s --max-time 15 -X POST "$MCP_BASE_URL" \
        -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        -H "Mcp-Session-Id: $1" \
        -d "{\"jsonrpc\":\"2.0\",\"id\":$2,\"method\":\"$3\",\"params\":$4}"
}

# mcp_tool_call SESSION ID TOOL ARGUMENTS_JSON — convenience wrapper for tools/call.
mcp_tool_call() {
    mcp_call "$1" "$2" tools/call "{\"name\":\"$3\",\"arguments\":$4}"
}
