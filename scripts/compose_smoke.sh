#!/usr/bin/env bash
# End-to-end check of the container stack: it starts everything, proves each claim the README
# makes about the deployment, and exits non-zero on the first disagreement.
#
#   bash scripts/compose_smoke.sh            # build, run, assert, keep running
#   bash scripts/compose_smoke.sh --cleanup  # assert, then tear down and remove volumes
set -uo pipefail
cd "$(dirname "$0")/.."

COMPOSE=(docker compose -f deploy/compose/docker-compose.yml)
# This suite asserts on answers, so it runs with the model service mocked on purpose: a live
# provider rewords a free-chat answer and the assertions turn into a test of the provider. The
# live path has its own script (scripts/check_nlp_live.sh) that says what it proves.
export MODEL_BASE_URL="" MODEL_NAME="" MODEL_API_KEY=""
GW=http://127.0.0.1:8090
# The demo tokens from config/dsh-tokens.json; they are demo values, stated as such in the file.
OWNER_TOKEN=dsh-demo-owner-token
DEV_TOKEN=dsh-demo-dev-token
UI=http://127.0.0.1:8088
fail=0

say()  { printf '\n== %s\n' "$*"; }
ok()   { printf '  ok   %s\n' "$*"; }
bad()  { printf '  FAIL %s\n' "$*"; fail=1; }

expect() { # expect <label> <expected substring> <actual>
  case "$3" in *"$2"*) ok "$1" ;; *) bad "$1 — wanted '$2', got '$3'" ;; esac
}

say "building and starting"
"${COMPOSE[@]}" up -d --build >/tmp/compose-up.log 2>&1 || { tail -20 /tmp/compose-up.log; exit 1; }

# Quota is shared state: a suite that asserts on answers must start with a fresh allowance, or a
# second run today fails on "daily quota reached" and looks like a broken gateway.
say "clearing today's usage counters"
for _ in $(seq 1 30); do
  if "${COMPOSE[@]}" exec -T postgres psql -U postgres -d agent -c 'DELETE FROM usage_counters' >/dev/null 2>&1; then
    # The gateway also holds counters in memory, so clearing the table alone leaves a process that
    # still believes today is spent. Restart it: the run then starts from a known zero.
    "${COMPOSE[@]}" restart gateway >/dev/null 2>&1
    ok "usage counters cleared (gateway restarted to drop its in-memory copy)"
    break
  fi
  sleep 2
done

say "waiting for the gateway"
for _ in $(seq 1 60); do
  curl -sf "$GW/health" >/dev/null 2>&1 && break
  sleep 2
done
curl -sf "$GW/health" >/dev/null 2>&1 && ok "gateway answers /health" || bad "gateway never came up"

say "seeding the user directory (Postgres, through the service)"
seed() { # seed <id> <true|false>
  curl -sf -X POST http://127.0.0.1:8093/mcp -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"user_set_active\",\"arguments\":{\"user_id\":\"$1\",\"active\":$2}}}" \
    | python3 -c "import json,sys;print(json.load(sys.stdin)['result']['content'][0]['text'])"
}
seed 1001 true  >/dev/null && seed 2002 false >/dev/null && seed 3003 true >/dev/null
expect "seed reported the postgres store" '"store": "postgres"' "$(seed 1001 true)"

say "waiting for the 30s directory refresh to reach the gateway"
# The gateway loads the directory at boot, which is before this script seeds it. Waiting for
# "source: userdir" alone used to pass immediately with zero users loaded, and every later check
# then failed for the wrong reason. Wait for the fact that matters: this person is known and active.
for _ in $(seq 1 30); do
  flags=$(curl -s "$GW/v1/whoami?user=1001&chat=dm" | python3 -c "import json,sys;j=json.load(sys.stdin).get('directory',{});print(f\"{j.get('known')}/{j.get('active')}\")" 2>/dev/null)
  [ "$flags" = "True/True" ] && break
  sleep 3
done
expect "the gateway knows and activates 1001" "True/True" "$flags"
src=$(curl -s "$GW/v1/whoami?user=1001&chat=dm" | python3 -c "import json,sys;print(json.load(sys.stdin).get('directory',{}).get('source',''))" 2>/dev/null)
expect "directory source is the service, not policy.json" "userdir" "$src"

post() { # post <user> <chat> <text> → http code, body in /tmp/smoke.json
  curl -s -o /tmp/smoke.json -w '%{http_code}' -X POST "$GW/v1/respond" \
    -d "{\"platform_user_id\":\"$1\",\"chat_type\":\"$2\",\"text\":\"$3\"}"
}
reply() { python3 -c "import json;d=json.load(open('/tmp/smoke.json'));print(d.get('chat_reply') or d.get('denied'))"; }

say "the four decisions"
code=$(post 1001 dm "status");            [ "$code" = 200 ] && ok "dm status 200" || bad "dm status $code"
expect "answer came from the Go tool service" "game=ok" "$(reply)"
code=$(post 1001 group_public "salary this month")
expect "a public group has no salary tool" "I have no tool for that." "$(reply)"
code=$(post 2002 dm "status");            [ "$code" = 403 ] && ok "disabled user 403" || bad "disabled user $code"
expect "disabled user is named" "user_disabled" "$(reply)"
code=$(post 9999 dm "status");            [ "$code" = 403 ] && ok "unknown user 403" || bad "unknown user $code"
expect "unknown user is named" "user_not_in_directory" "$(reply)"

say "a tool served by the TypeScript process"
code=$(post 1001 dm "wiki adr")
expect "wiki_search answered across languages" "Result:" "$(reply)"

say "queued extraction over the compose network"
if docker cp services/ocr/samples/text.pdf "$("${COMPOSE[@]}" ps -q ocr)":/app/samples/text.pdf 2>/dev/null; then
  code=$(post 1001 dm "read samples/text.pdf")
  expect "extraction was queued, not blocked" "Extraction queued as" "$(reply)"
  sleep 3
  code=$(post 1001 dm "result")
  expect "the job was read back by the gateway" "Extraction finished" "$(reply)"
else
  printf '  skip OCR sample not available (run make samples first)\n'
fi

say "the console container serves the UI and proxies the API"
curl -sf "$UI/" >/tmp/console.html 2>/dev/null
expect "console app is served" "Agent platform" "$(cat /tmp/console.html 2>/dev/null)"
curl -sf "$UI/health" >/dev/null 2>&1 && ok "nginx proxies /health to the gateway" \
  || bad "nginx proxy for /health is missing"

say "a role change from the console applies to the next MCP call"
ADMIN=dsh-demo-owner-token; DEV=dsh-demo-dev-token
mcp_call() { # mcp_call <token> <tool> — wiki_search exists in this stack and guest lacks it
  curl -s -X POST "$UI/mcp" -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$2\",\"arguments\":{\"q\":\"smoke\"}}}"
}
before=$(mcp_call "$DEV" wiki_search)
case "$before" in *'"result"'*) ok "dev may use wiki_search before the change" ;; *) bad "dev was already denied: $before" ;; esac
code=$(curl -s -o /tmp/role.json -w '%{http_code}' -X POST "$UI/admin/users/2002/role" \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' -d '{"role":"guest"}')
expect "the console API accepted the change" "200" "$code"
after=$(mcp_call "$DEV" wiki_search)
case "$after" in *tool_not_in_role*) ok "the demoted token is refused on the next call" ;;
  *) bad "demotion did not take effect: $after" ;; esac
curl -s -o /dev/null -X POST "$UI/admin/users/2002/role" -H "Authorization: Bearer $ADMIN" \
  -H 'Content-Type: application/json' -d '{"role":"dev"}'
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$UI/admin/users/2002/role" \
  -H "Authorization: Bearer $DEV" -H 'Content-Type: application/json' -d '{"role":"owner"}')
expect "a non-admin cannot assign roles" "403" "$code"

say "operational surface: metrics, validation, per-tool deadlines"
curl -sf "$GW/metrics" >/tmp/metrics.txt 2>/dev/null
expect "Prometheus metrics are exposed" "gateway_mcp_cache_misses_total" "$(cat /tmp/metrics.txt 2>/dev/null)"
case "$(cat /tmp/metrics.txt 2>/dev/null)" in *"1001"*) bad "metrics leak user ids" ;; *) ok "metrics carry no user ids" ;; esac
bad_args=$(curl -s -X POST "$UI/mcp" -H "Authorization: Bearer dsh-demo-owner-token" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wiki_search","arguments":{"q":42}}}')
expect "a wrong argument type is refused at the gateway" "invalid arguments" "$bad_args"

say "the per-user bucket refuses a burst, and says how long to wait"
burst=$(python3 - "$UI" <<'PYEOF'
import json, sys, urllib.request
from concurrent.futures import ThreadPoolExecutor
url = sys.argv[1]
def one(i):
    body = json.dumps({"jsonrpc":"2.0","id":i,"method":"tools/call",
                       "params":{"name":"status","arguments":{}}}).encode()
    req = urllib.request.Request(url + "/mcp", data=body, method="POST",
        headers={"Content-Type":"application/json","Authorization":"Bearer dsh-demo-owner-token"})
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            return "rate" if "rate limited" in r.read().decode() else "ok"
    except Exception:
        return "err"
with ThreadPoolExecutor(max_workers=25) as ex:
    out = list(ex.map(one, range(25)))
print(f"{out.count('ok')} {out.count('rate')}")
PYEOF
)
refused=$(echo "$burst" | cut -d' ' -f2)
[ "${refused:-0}" -ge 1 ] && ok "burst of 25 → $refused refused by the bucket" \
  || bad "the bucket did not refuse anything: $burst"

say "quota counters are shared through postgres, and survive a restart"
used_before=$(curl -s "$GW/v1/whoami?user=1001&chat=dm" | python3 -c "import json,sys;print(json.load(sys.stdin)['quota']['requests_used'])")
"${COMPOSE[@]}" restart gateway >/dev/null 2>&1
for _ in $(seq 1 40); do curl -sf "$GW/health" >/dev/null 2>&1 && break; sleep 2; done
used_after=$(curl -s "$GW/v1/whoami?user=1001&chat=dm" | python3 -c "import json,sys;print(json.load(sys.stdin)['quota']['requests_used'])")
if [ "${used_after:-0}" -ge "${used_before:-0}" ] && [ "${used_after:-0}" -gt 0 ]; then
  ok "a restart kept the day's counters ($used_before → $used_after, not reset to zero)"
else
  bad "counters did not survive the restart: $used_before → $used_after"
fi

say "an analytics question reaches ClickHouse through the gateway"
TOOL_CALL() {  # token, tool, args-json
  curl -s --max-time 30 -X POST "$GW/mcp" -H 'Content-Type: application/json' \
    -H "Authorization: Bearer $1" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$2\",\"arguments\":$3}}"
}
SCHEMA=$(TOOL_CALL "$OWNER_TOKEN" ch_schema '{}' | python3 -c "
import json,sys
d=json.load(sys.stdin)['result']
print(d['content'][0]['text'] if not d.get('isError') else 'ERR ' + d['content'][0]['text'])")
if printf '%s' "$SCHEMA" | grep -q '"events'; then
  ok "ch_schema lists the seeded tables"
else
  bad "ch_schema did not return the seeded schema: $SCHEMA"
fi

ROWS=$(TOOL_CALL "$OWNER_TOKEN" ch_query '{"sql":"SELECT event, count() AS n FROM events GROUP BY event ORDER BY n DESC LIMIT 3"}' | python3 -c "
import json,sys
d=json.load(sys.stdin)['result']
if d.get('isError'):
    print('ERR ' + d['content'][0]['text'])
else:
    b=json.loads(d['content'][0]['text']); print(b['row_count'])")
if [ "${ROWS:-0}" -ge 1 ] 2>/dev/null; then
  ok "ch_query returned $ROWS rows from ClickHouse"
else
  bad "ch_query did not return rows: $ROWS"
fi

REJECTED=$(TOOL_CALL "$OWNER_TOKEN" ch_query '{"sql":"DROP TABLE events"}' | grep -c "sql_rejected" || true)
if [ "${REJECTED:-0}" -ge 1 ]; then
  ok "a mutation is refused by the guard before it reaches the database"
else
  bad "ch_query accepted a DROP"
fi

DEV_TOOLS=$(curl -s -X POST "$GW/mcp" -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $DEV_TOKEN" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' \
  | python3 -c "import json,sys;print(' '.join(t['name'] for t in json.load(sys.stdin)['result']['tools']))")
if printf '%s' "$DEV_TOOLS" | grep -q ch_schema && ! printf '%s' "$DEV_TOOLS" | grep -q ch_query; then
  ok "raw SQL is owner-only: dev sees ch_schema + nlp_ask, not ch_query"
else
  bad "role separation for ClickHouse tools is wrong: $DEV_TOOLS"
fi
say "the OpenAI-shaped model door"
models=$(curl -s "$GW/v1/models" -H "Authorization: Bearer $OWNER_TOKEN")
expect "the model list is the whitelist" '"id":"gpt-4.1' "$models"
code=$(curl -s -o /tmp/oa-denied.json -w '%{http_code}' "$GW/v1/chat/completions" \
  -H "Authorization: Bearer $OWNER_TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"not-in-the-whitelist","messages":[{"role":"user","content":"hi"}]}')
expect "a model outside the whitelist is refused" "404" "$code"
expect "the refusal names the reason" "model_not_found" "$(cat /tmp/oa-denied.json)"
code=$(curl -s -o /dev/null -w '%{http_code}' "$GW/v1/chat/completions" \
  -H 'Content-Type: application/json' -d '{"model":"gpt-4.1-mini","messages":[]}')
expect "no token is not a user" "401" "$code"

say "self-service credentials"
me=$(curl -s "$GW/me" -H "Authorization: Bearer $OWNER_TOKEN")
expect "a token says who it is" '"user":"1001"' "$me"
expect "and shows a mask, never the token" 'dsh-…oken' "$me"
tokens=$(curl -s "$GW/admin/tokens" -H "Authorization: Bearer $OWNER_TOKEN")
expect "the admin list is masked" '…' "$tokens"
case "$tokens" in
  *"$OWNER_TOKEN"*) bad "the token list reproduced a token" ;;
  *) ok "the token list reproduces no token" ;;
esac
code=$(curl -s -o /dev/null -w '%{http_code}' "$GW/admin/tokens" -H "Authorization: Bearer $DEV_TOKEN")
expect "a non-admin cannot list tokens" "403" "$code"

say "one audit view for every gateway"
curl -sf "http://127.0.0.1:8096/v1/report?day=$(date +%F)" > /tmp/smoke-report.json \
  && python3 -c "
import json;d=json.load(open('/tmp/smoke-report.json'))
assert d['events'] >= 6, d
assert d['users'].get('1001', 0) >= 3, d
assert d['decisions'].get('denied', 0) >= 2, d
print('  ok   audit sink aggregated', d['events'], 'events,', len(d['users']), 'users')" \
  || bad "audit sink report missing or wrong"

if [ "${1:-}" = "--cleanup" ]; then
  say "tearing down"
  "${COMPOSE[@]}" down -v >/dev/null 2>&1 && ok "stack removed"
fi

say "result"
[ "$fail" = 0 ] && { echo "  ALL CHECKS PASSED"; exit 0; } || { echo "  FAILURES PRESENT"; exit 1; }
