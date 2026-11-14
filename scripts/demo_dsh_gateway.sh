#!/usr/bin/env bash
# End-to-end demo: DeepSeek Harness (or any MCP client) → gateway (RBAC + cache + audit) → jira-mock.
# Evidence only: every number printed here comes from the running processes, not from a description.
set -uo pipefail
cd "$(dirname "$0")/.."

BIN=services/gateway-go
STATE=$(mktemp -d)
OWNER_TOKEN=dsh-demo-owner-token
DEV_TOKEN=dsh-demo-dev-token
URL=http://127.0.0.1:8090/mcp

cleanup() { kill $JIRA_PID $GW_PID 2>/dev/null; wait 2>/dev/null; }
trap cleanup EXIT

(cd "$BIN" && go build -o "$STATE/jira-mock" ./cmd/jira-mock && go build -o "$STATE/gateway" .) || exit 1
"$STATE/jira-mock" -addr 127.0.0.1:8092 & JIRA_PID=$!
"$STATE/gateway" -addr 127.0.0.1:8090 -mcp config/mcp.json -tools config/tools.json \
  -policy services/gateway/config/policy.json -consent config/consent.json \
  -tokens config/dsh-tokens.json -audit "$STATE/audit.log" -refresh 0 >"$STATE/gw.log" 2>&1 & GW_PID=$!
sleep 1.2

rpc() { # $1 token, $2 json body
  curl -sS -H "Authorization: Bearer $1" -H 'Content-Type: application/json' -d "$2" "$URL"
}

echo "── 1. initialize (owner) ─────────────────────────────────────────"
rpc $OWNER_TOKEN '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"demo","version":"0"}}}' | head -c 400; echo

echo; echo "── 2. tools/list: owner vs dev (what the model is even told about) ─"
OWNER_TOOLS=$(rpc $OWNER_TOKEN '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | python3 -c 'import sys,json;print(" ".join(t["name"] for t in json.load(sys.stdin)["result"]["tools"]))')
DEV_TOOLS=$(rpc $DEV_TOKEN '{"jsonrpc":"2.0","id":3,"method":"tools/list"}' | python3 -c 'import sys,json;print(" ".join(t["name"] for t in json.load(sys.stdin)["result"]["tools"]))')
echo "owner: $OWNER_TOOLS"
echo "dev  : $DEV_TOOLS"
echo "chỉ owner thấy: $(comm -23 <(tr ' ' '\n' <<<"$OWNER_TOOLS"|sort -u) <(tr ' ' '\n' <<<"$DEV_TOOLS"|sort -u) | tr '\n' ' ')"

echo; echo "── 3. cache: cùng truy vấn 2 lần ─────────────────────────────────"
for i in 1 2; do
  rpc $OWNER_TOKEN '{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"jira_search","arguments":{"query":"platform"}}}' \
    | python3 -c 'import sys,json;d=json.load(sys.stdin);print("  call '"$i"':", d["result"]["content"][0]["text"][:80].replace("\n"," "))'
done

echo; echo "── 4. RBAC: dev gọi jira_create_issue ───────────────────────────"
rpc $DEV_TOKEN '{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"jira_create_issue","arguments":{"summary":"dev tries to write"}}}'; echo

echo; echo "── 5. owner gọi jira_create_issue (được phép, ttl=0 nên không cache) ─"
rpc $OWNER_TOKEN '{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"jira_create_issue","arguments":{"summary":"gateway demo issue","team":"platform"}}}' | head -c 300; echo

echo; echo "── 6. /stats/mcp ────────────────────────────────────────────────"
curl -sS http://127.0.0.1:8090/stats/mcp | python3 -m json.tool

echo; echo "── 7. audit (dòng thật, do gateway ghi) ─────────────────────────"
tail -n 6 "$STATE/audit.log"
echo; echo "state: $STATE"
