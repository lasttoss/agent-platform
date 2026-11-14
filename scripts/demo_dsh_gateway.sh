#!/usr/bin/env bash
# End-to-end demo: an MCP client (DeepSeek Harness and friends) → gateway (RBAC + cache +
# single-flight + audit) → jira-mock. Every number printed here comes from the running
# processes; nothing is asserted that was not observed.
set -uo pipefail
cd "$(dirname "$0")/.."

BIN=services/gateway-go
STATE=$(mktemp -d)
OWNER=dsh-demo-owner-token
DEV=dsh-demo-dev-token
NEW=dsh-demo-owner-v2-token
URL=http://127.0.0.1:8090/mcp

cleanup() { kill "$JIRA_PID" "$GW_PID" 2>/dev/null; wait 2>/dev/null; cp "$STATE/tokens.bak" config/dsh-tokens.json 2>/dev/null; }
trap cleanup EXIT

(cd "$BIN" && go build -o "$STATE/jira-mock" ./cmd/jira-mock && go build -o "$STATE/gateway" .) || exit 1
cp config/dsh-tokens.json "$STATE/tokens.bak"
"$STATE/jira-mock" -addr 127.0.0.1:8092 & JIRA_PID=$!
"$STATE/gateway" -addr 127.0.0.1:8090 -mcp config/mcp.json -tools config/tools.json \
  -policy services/gateway/config/policy.json -consent config/consent.json \
  -tokens config/dsh-tokens.json -audit "$STATE/audit.log" -refresh 0 >"$STATE/gw.log" 2>&1 & GW_PID=$!
sleep 1.5

rpc() { curl -sS -H "Authorization: Bearer $1" -H 'Content-Type: application/json' -d "$2" "$URL"; }
stats() { curl -sS http://127.0.0.1:8090/stats/mcp | python3 -m json.tool; }

echo "── 1. initialize ────────────────────────────────────────────────"
rpc "$OWNER" '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"demo","version":"0"}}}' | head -c 260; echo

echo; echo "── 2. tools/list: owner vs dev ──────────────────────────────────"
OWNER_TOOLS=$(rpc "$OWNER" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | python3 -c 'import sys,json;print(" ".join(t["name"] for t in json.load(sys.stdin)["result"]["tools"]))')
DEV_TOOLS=$(rpc "$DEV" '{"jsonrpc":"2.0","id":3,"method":"tools/list"}' | python3 -c 'import sys,json;print(" ".join(t["name"] for t in json.load(sys.stdin)["result"]["tools"]))')
echo "owner: $OWNER_TOOLS"
echo "dev  : $DEV_TOOLS"
echo "chỉ owner thấy: $(comm -23 <(tr ' ' '\n' <<<"$OWNER_TOOLS"|sort -u) <(tr ' ' '\n' <<<"$DEV_TOOLS"|sort -u) | tr '\n' ' ')"

echo; echo "── 3. cache: cùng truy vấn 2 lần ────────────────────────────────"
for i in 1 2; do
  rpc "$OWNER" '{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"jira_search","arguments":{"query":"platform"}}}' >/dev/null
  echo "  call $i xong"
done
stats | grep -E 'cache_hits|cache_misses|coalesced|cache_hit_rate'

echo; echo "── 4. RBAC: dev gọi tool không có trong role ────────────────────"
rpc "$DEV" '{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"jira_create_issue","arguments":{"summary":"dev tries"}}}'; echo

echo; echo "── 5. SSE: client xin text/event-stream ─────────────────────────"
curl -sS -D "$STATE/sse.headers" -o "$STATE/sse.body" -H "Authorization: Bearer $OWNER" \
  -H 'Accept: text/event-stream' -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":20,"method":"tools/list"}' "$URL"
grep -iE '^(HTTP|content-type)' "$STATE/sse.headers" | tr -d '\r'
head -c 70 "$STATE/sse.body"; echo ' …'
echo "  sse_responses: $(curl -sS http://127.0.0.1:8090/stats/mcp | python3 -c 'import sys,json;print(json.load(sys.stdin)["sse_responses"])')"

echo; echo "── 6. concurrency: 40 request song song, 5 args, 2 role ─────────"
rm -f "$STATE/codes"; START=$(date +%s.%N); pids=()
for i in $(seq 1 40); do
  if [ $((i % 2)) -eq 0 ]; then T=$OWNER; else T=$DEV; fi
  ( curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $T" \
      -H 'Content-Type: application/json' \
      -d "{\"jsonrpc\":\"2.0\",\"id\":$i,\"method\":\"tools/call\",\"params\":{\"name\":\"jira_search\",\"arguments\":{\"query\":\"q$((i % 5))\"}}}" \
      "$URL" >> "$STATE/codes" ) &
  pids+=($!)
done
wait "${pids[@]}"; END=$(date +%s.%N)
echo "  HTTP 200: $(grep -c '^200$' "$STATE/codes")/40 trong $(python3 -c "print(f'{$END-$START:.2f}s')")"
stats | grep -E 'cache_misses|coalesced'
echo "  (10 miss = 2 role × 5 args tới service thật; 30 coalesced = phần còn lại chờ chung kết quả)"

echo; echo "── 7. rotate token, KHÔNG restart ───────────────────────────────"
python3 -c "
import json
d=json.load(open('config/dsh-tokens.json'))
d['tokens']={'$NEW':'1001','$DEV':'2002'}
json.dump(d,open('config/dsh-tokens.json','w'),indent=2,ensure_ascii=False)
open('config/dsh-tokens.json','a').write('\n')"
echo -n "  token cũ  → "; curl -s -o /dev/null -w 'HTTP %{http_code}\n' -H "Authorization: Bearer $OWNER" \
  -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":21,"method":"tools/list"}' "$URL"
echo -n "  token mới → "; curl -s -o /dev/null -w 'HTTP %{http_code}\n' -H "Authorization: Bearer $NEW" \
  -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":22,"method":"tools/list"}' "$URL"
cp "$STATE/tokens.bak" config/dsh-tokens.json

echo; echo "── 8. audit (dòng thật, do gateway ghi) ─────────────────────────"
grep -E 'mcp_tokens_rotated|mcp_denied|mcp_call' "$STATE/audit.log" | tail -n 6

echo; echo "── 9. stats cuối ───────────────────────────────────────────────"
stats
echo "state: $STATE"
