#!/usr/bin/env bash
# Install this platform into a dsh profile, one command, no interactive setup.
#
#   ./scripts/dsh_bootstrap.sh [profile-name]
#
# The setup is files, so it is copied rather than retyped. Three layers, none of them interactive:
#
#   bundle    deploy/dsh/bundle — an npm package whose package.json says
#             "dsh": {"bundle": {"patch": "./cordis.patch.yml"}}. That one line is the whole
#             install story for another machine: the profile lists it in dsh.profile.bundles.
#   profile   a directory: package.json lists the bundles, cordis.patch.yml is your own layer
#   env       only the per-machine parts: endpoint, token, model
#
# The --patch route still exists for a machine you would rather not install into; it carries the
# same file, so the two cannot drift.
set -euo pipefail

PROFILE="${1:-platform}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
BUNDLE="$REPO/deploy/dsh/bundle"
DSH="${DSH:-dsh}"
DSH_HOME_DIR="${DSH_HOME:-$HOME/.dsh}"
command -v "$DSH" >/dev/null || { echo "dsh not on PATH; try: npx -y @deepseek-ai/dsh@latest $*" >&2; exit 2; }

# 1. the profile, from the shipped template. --dump-config is what makes this create-and-exit;
#    without it the harness boots and sits there.
if [ ! -d "$DSH_HOME_DIR/profiles/$PROFILE" ]; then
  DSH_HOME="$DSH_HOME_DIR" "$DSH" "$PROFILE" --from-default-profile headless --dump-config >/dev/null
  echo "profile $PROFILE created from the shipped headless template"
else
  echo "profile $PROFILE already exists; leaving its base layers alone"
fi

# 2. the bundle. `add` records it in dsh.profile.bundles as well, so the run command needs no flags.
#    -w because a profile is a pnpm workspace root; without it pnpm refuses with
#    ERR_PNPM_ADDING_TO_ROOT, which reads like a policy problem and is not one.
if DSH_HOME="$DSH_HOME_DIR" "$DSH" plugin --profile "$PROFILE" add -w "file:$BUNDLE" >/dev/null 2>&1; then
  echo "bundle @lasttoss/dsh-platform installed into $PROFILE"
else
  # Fallback for a machine with no pnpm, or one where installing is not allowed: merge the same
  # overlay into the profile's own layer. The shipped template ends with a lone `[]`, so appending
  # makes a second YAML document and dsh refuses it — the `[]` has to be replaced.
  python3 - "$DSH_HOME_DIR/profiles/$PROFILE/cordis.patch.yml" "$BUNDLE/cordis.patch.yml" <<'PY'
import sys
layer, overlay = sys.argv[1], sys.argv[2]
kept = [l for l in open(layer).read().splitlines() if l.strip() != '[]']
body = open(overlay).read().splitlines()
if any('dsh as a client of this platform' in l for l in kept):
    print('overlay already in the profile layer; not adding it twice')
else:
    open(layer, 'w').write('\n'.join(kept + body) + '\n')
    print('bundle install unavailable; overlay merged into the profile layer instead')
PY
fi

# 3. a model the endpoint actually serves. /v1/models says what the gateway ALLOWS, which need not
#    be what the provider SERVES: the difference shows up as a 400 inside a task, reading like a
#    harness bug. So ask for the list, then make one 1-token call per model, first answer wins.
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

# 4. verify the composed tree without booting anything: both plugins in the dump means the profile
#    can reach the model door and the tool door; where they came from is printed by --dump-config.
dump="$(OPENAI_BASE_URL="$BASE" OPENAI_API_KEY="${OPENAI_API_KEY:-x}" OPENAI_MODEL="$MODEL" \
        DSH_HOME="$DSH_HOME_DIR" "$DSH" "$PROFILE" --dump-config)"
for want in llm-pi-ai mcp-platform; do
  grep -q "id: $want" <<<"$dump" || { echo "composed config is missing $want" >&2; exit 1; }
done
echo "composed config has the model adapter and the platform MCP client"

cat <<EOF

Export these, then run:
  export OPENAI_BASE_URL=$BASE
  export OPENAI_API_KEY=<token from console → Connect>
  export PLATFORM_TOKEN=\$OPENAI_API_KEY
  export OPENAI_MODEL=$MODEL

  dsh $PROFILE "list the tables in the analytics database"
EOF
