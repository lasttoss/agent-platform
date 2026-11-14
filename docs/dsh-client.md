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
export COMMANDCODE_KEY=...            # any OpenAI-compatible key; the adapter reads it per request

dsh --profile headless --patch deploy/dsh/dsh-platform.patch.yml \
  "Gọi tool mcp__platform__nlp_ask với câu hỏi: 'Có bao nhiêu người chơi đến từ Việt Nam?'"
```

## What the overlay says, and why

- **`llm-pi-ai` inserted, `llm-deepseek` disabled.** The DeepSeek adapter speaks the official
  DeepSeek wire format and it does not fit this endpoint: pointed at
  `https://api.commandcode.ai/provider/v1` it posts to a route the provider does not serve
  (`404`), and at `/provider/v1/chat/completions` it reports *"DeepSeek Messages request failed"*.
  pi-ai is the generic adapter — a route with `api: openai-completions` and a model list is
  configuration, not code.
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
