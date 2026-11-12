#!/usr/bin/env bash
# End-to-end check of the container stack: it starts everything, proves each claim the README
# makes about the deployment, and exits non-zero on the first disagreement.
#
#   bash scripts/compose_smoke.sh            # build, run, assert, keep running
#   bash scripts/compose_smoke.sh --cleanup  # assert, then tear down and remove volumes
set -uo pipefail
cd "$(dirname "$0")/.."

COMPOSE=(docker compose -f deploy/compose/docker-compose.yml)
GW=http://127.0.0.1:8090
fail=0

say()  { printf '\n== %s\n' "$*"; }
ok()   { printf '  ok   %s\n' "$*"; }
bad()  { printf '  FAIL %s\n' "$*"; fail=1; }

expect() { # expect <label> <expected substring> <actual>
  case "$3" in *"$2"*) ok "$1" ;; *) bad "$1 — wanted '$2', got '$3'" ;; esac
}

say "building and starting"
"${COMPOSE[@]}" up -d --build >/tmp/compose-up.log 2>&1 || { tail -20 /tmp/compose-up.log; exit 1; }

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
for _ in $(seq 1 20); do
  src=$(curl -s "$GW/v1/whoami?user=1001&chat=dm" | python3 -c "import json,sys;print(json.load(sys.stdin).get('directory',{}).get('source',''))" 2>/dev/null)
  [ "$src" = "userdir" ] && break
  sleep 3
done
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
