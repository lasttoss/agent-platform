# dsh as a client of this platform

The platform is MCP all the way down; a real agent harness is the honest test of that claim.
[DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) (`dsh`) is one: its MCP client
registers an external server's tools under `mcp__<server>__<tool>`, so it sees this gateway's tools
exactly as the gateway intends — filtered by the caller's role before the model is told anything.

## Run it

```bash
# token: the KEY in config/dsh-tokens.json ("dsh-demo-owner-token" → user 1001).
# Not the value "1001": that is the user id, and passing it yields a 401.
export PLATFORM_TOKEN=dsh-demo-owner-token
export OPENAI_BASE_URL=https://api.openai.com/v1   # any OpenAI-compatible endpoint
export OPENAI_API_KEY=...                          # read per request, never from a file
export OPENAI_MODEL=gpt-4.1-mini                   # a model that endpoint serves

dsh --profile headless --patch deploy/dsh/dsh-platform.patch.yml \
  "Gọi tool mcp__platform__nlp_ask với câu hỏi: 'Có bao nhiêu người chơi đến từ Việt Nam?'"
```

## What the overlay says, and why

- **`llm-pi-ai` inserted, `llm-deepseek` disabled.** The DeepSeek adapter speaks the official
  DeepSeek wire format and does not fit an OpenAI-shaped endpoint: it posts to a route such a
  provider does not serve, and appending the chat path moves the failure one hop deeper instead of
  fixing it. pi-ai is the generic adapter — a route with `api: openai-completions` and a model list
  is configuration, not code.
  The inserted row keeps the id **`llm-pi-ai`**: a second pi-ai instance under a different id fails
  at load with *configurable provider "amazon-bedrock" is already declared*, which reads like a
  catalog problem and is really "two adapters, one registry".
- **`agent-default-model` points at the new route.** The headless runner creates one agent and
  reads its selection from this service, so a route nobody selects is a route nobody uses.
- **`mcp-platform`** points at the gateway, `streamable-http`, `Authorization: Bearer`, and
  `failOnStartupError: true` — a demo that silently runs without its MCP server proves nothing.
- **No secret in the file**: `apiKeyEnv` and the header template reference environment variables.

## What it proved, measured

```
dsh → gateway :8090 → clickhouse-mcp :8098 → model → ClickHouse
audit: {event: mcp_call, tool: nlp_ask, user: "1001", cache: bypass, ms: 1962}
answer: SELECT count() FROM players WHERE country IN ('VN','Vietnam','Việt Nam') → 4
```

Two behaviours worth keeping, both observed rather than assumed:

- Asked about a table that does not exist (`events_daily` in the small demo database), the model
  returned `NONE` and `nlp_ask` refused with `no_table_can_answer_this`. No SQL was invented, and
  the agent reported the refusal instead of guessing.
- When `nlp_ask` was unavailable (model service unconfigured), the same agent noticed, called
  `ch_schema` and `ch_query` instead, and answered from the rows — the fallback is visible in the
  gateway audit as three separate `mcp_call` events, which is the point of putting the door in one
  place.
