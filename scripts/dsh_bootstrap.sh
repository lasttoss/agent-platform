#!/usr/bin/env bash
# Install this platform into a dsh profile: one command, no interactive setup, nothing typed twice.
#
#   ./scripts/dsh_bootstrap.sh [profile-name]
#
# What "installing dsh on a machine" otherwise costs: a fresh profile from a shipped template, then
# every plugin declared by hand. This does the same thing from files that live in the repository,
# so the machine is a copy of the repo rather than a set of remembered answers.
#
# Three layers, none of them interactive:
#   profile    a directory: package.json lists the bundles, cordis.patch.yml is your layer
#   patch      deploy/dsh/dsh-platform.patch.yml — the entire "everything at once" (LLM adapter,
#              MCP client, default model) as one list of patch entries
#   env        the per-machine parts only: endpoint, token, model
set -euo pipefail

PROFILE="${1:-platform}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
DSH="${DSH:-dsh}"
DSH_HOME_DIR="${DSH_HOME:-$HOME/.dsh}"
command -v "$DSH" >/dev/null || { echo "dsh not on PATH; try: npx -y @deepseek-ai/dsh@latest $*" >&2; exit 2; }

# 1. the profile: created from the shipped template, then --dump-config exits instead of booting.
#    Without --dump-config this call would boot the harness and sit there.
if [ ! -d "$DSH_HOME_DIR/profiles/$PROFILE" ]; then
  DSH_HOME="$DSH_HOME_DIR" "$DSH" "$PROFILE" --from-default-profile headless --dump-config >/dev/null
  echo "profile $PROFILE created from the shipped headless template"
else
  echo "profile $PROFILE already exists; leaving the base layers alone"
fi

# 2. the patch: merged into the profile's own layer, so dsh needs no flags at run time.
#    The shipped template ends with a lone `[]`; appending after it makes a second YAML document and
#    dsh refuses to parse the overlay ("end of the stream or a document separator is expected").
python3 - "$DSH_HOME_DIR/profiles/$PROFILE/cordis.patch.yml" "$REPO/deploy/dsh/dsh-platform.patch.yml" <<'PY'
import sys
layer, overlay = sys.argv[1], sys.argv[2]
template = [l for l in open(layer).read().splitlines() if l.strip() != '[]']
body = open(overlay).read().splitlines()
marker = 'dsh as a client of this platform'
if any(marker in l for l in template):
    print('patch overlay already merged; not adding it twice')
else:
    open(layer, 'w').write('\n'.join(template + body) + '\n')
    print(f'overlay merged into {layer}')
PY

# 3. pick a model the endpoint actually serves. A hardcoded default is a guess: the platform's
#    policy default need not be a model the upstream provider offers, and the failure is a 400 that
#    reads like a harness bug ("model_unavailable: HTTP Error 400").
#    /v1/models says what the gateway ALLOWS, not what the provider SERVES — those differ, and the
#    difference surfaces as a 400 deep inside a task. So: ask for the list, then make one 1-token
#    call per model and take the first that answers. A bootstrap that ends with "and now try it" is
#    a bootstrap that ends with a support question.
BASE="${OPENAI_BASE_URL:-http://127.0.0.1:8090/v1}"
MODEL="${OPENAI_MODEL:-}"
if [ -z "$MODEL" ] && [ -n "${OPENAI_API_KEY:-}" ]; then
  for candidate in $(curl -sf "$BASE/models" -H "Authorization: Bearer $OPENAI_API_KEY" \
      | python3 -c 'import json,sys; d=json.load(sys.stdin); print(" ".join(m["id"] for m in d.get("data",[])))' 2>/dev/null || true); do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 40 -X POST "$BASE/chat/completions" \
      -H "Authorization: Bearer $OPENAI_API_KEY" -H 'Content-Type: application/json' \
      -d "{\"model\":\"$candidate\",\"max_tokens\":1,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" || true)"
    if [ "$code" = "200" ]; then MODEL="$candidate"; echo "model that answered a 1-token probe: $MODEL"; break; fi
    echo "model $candidate answered $code; trying the next one" >&2
  done
fi
MODEL="${MODEL:-gpt-4.1-mini}"

# 4. verify the composed tree without booting anything: if the two plugins are in the dump, the: if the two plugins are in the dump, the
#    profile can reach the model door and the tool door.
dump="$(OPENAI_BASE_URL="${OPENAI_BASE_URL:-http://127.0.0.1:8090/v1}" \
        OPENAI_API_KEY="${OPENAI_API_KEY:-x}" OPENAI_MODEL="$MODEL" \
        DSH_HOME="$DSH_HOME_DIR" "$DSH" "$PROFILE" --dump-config)"
for want in llm-pi-ai mcp-platform; do
  grep -q "id: $want" <<<"$dump" || { echo "composed config is missing $want" >&2; exit 1; }
done
echo "composed config has the model adapter and the platform MCP client"

cat <<EOF

Export these, then run:
  export OPENAI_BASE_URL=${OPENAI_BASE_URL:-http://127.0.0.1:8090/v1}
  export OPENAI_API_KEY=<token from console → Connect>
  export PLATFORM_TOKEN=\$OPENAI_API_KEY
  export OPENAI_MODEL=$MODEL

  dsh $PROFILE "list the tables in the analytics database"
EOF
