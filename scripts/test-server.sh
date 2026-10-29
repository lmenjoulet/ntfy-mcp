TEST_NTFY_SERVER="${TEST_NTFY_SERVER:-'https://ntfy.sh'}"
TEST_NTFY_TOPIC="${TEST_NTFY_TOPIC:-'agent'}"
TEST_NTFY_MESSAGE="${TEST_NTFY_MESSAGE:-'test message'}"
# Don't forget to set TEST_NTFY_TOKEN !

SESSION=$(curl -si -X POST http://127.0.0.1:12120/mcp \
     -H "Content-Type: application/json" \
     -H "Accept: application/json, text/event-stream" \
     -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}' \
     | grep -i mcp-session-id | tr -d '\r' | awk '{print $2}')
echo "Session : $SESSION"

curl -s -X POST http://127.0.0.1:12120/mcp \
     -H "Content-Type: application/json" \
     -H "Accept: application/json, text/event-stream" \
     -H "Mcp-Session-Id: $SESSION" \
     -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'

curl -s -X POST http://127.0.0.1:12120/mcp \
     -H "Content-Type: application/json" \
     -H "Accept: application/json, text/event-stream" \
     -H "Mcp-Session-Id: $SESSION" \
     -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"send_notification","arguments":{"base_url":"'$TEST_NTFY_SERVER'","message":"'$TEST_NTFY_MESSAGE'","title":"MCP test","topic":"'$TEST_NTFY_TOPIC'","token":"'$TEST_NTFY_TOKEN'","priority":3}}}'
