# dsh as a client of this platform

The platform is MCP all the way down; a real agent harness is the honest test of that claim.
[DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) (`dsh`) is one: its MCP client
registers an external server's tools under `mcp__<server>__<tool>`, so it sees this gateway's tools
exactly as the gateway intends — filtered by the caller's role before the model is told anything.

## Run it

One credential. Both doors — the model and the tools — take the same platform token:

```bash
# The token is the KEY in config/dsh-tokens.json ("dsh-demo-owner-token" → user 1001), not the
# user id "1001": passing the id yields a 401 with no hint why.
TOKEN=dsh-demo-owner-token
export PLATFORM_TOKEN=$TOKEN
export OPENAI_BASE_URL=http://127.0.0.1:8090/v1   # the gateway, not the provider
export OPENAI_API_KEY=$TOKEN                      # the same token authorizes model calls
export OPENAI_MODEL=deepseek/deepseek-v4.1-flash  # must be in the policy whitelist

dsh --profile headless --patch deploy/dsh/dsh-platform.patch.yml \
  "Gọi tool mcp__platform__nlp_ask với câu hỏi: 'Có bao nhiêu người chơi đến từ Việt Nam?'"
```

The provider key never leaves the server: `POST /v1/chat/completions` holds it, the caller holds a
token. Point `OPENAI_BASE_URL` at a provider directly instead and each person needs their own
provider account — which is the thing this endpoint exists to avoid.

## Where the token comes from

Two ways, both honest about the same rule: a token is shown **once**, and a lost token is
**rotated**, never revealed again.

```
console → Connect          your own token (masked), "Rotate my token" → new token + a
                           ready-to-paste snippet; the tab adopts the new token immediately,
                           because the old one stops working on the *next* request
console → Tokens (admin)   issue a token for a provisioned user, revoke anybody's; revoking
                           your own is refused with 409 and points at rotate
by hand                    edit the file and the mtime watcher puts it into force — no restart
```

The live file is state, not a checkout file. In the container it lives on the state volume
(`/state/tokens.json`), seeded once from `config/dsh-tokens.json`; that is why rotation works
without the gateway holding write access to the repository. Rotating the mounted *file* instead
fails with `open /.tokens-….json: permission denied` — a rename needs a writable directory next to
the target, which a single-file bind mount does not provide. Found by trying it.

## Two doors, one identity, one bill

```
/v1/chat/completions  bearer token → user → model service → provider (key stays here)
/mcp                  bearer token → user → role → tools
```
Measured in one run, from the gateway audit rather than from a transcript:

```
{event: openai_chat, user: "1001", model: "deepseek/deepseek-v4.1-flash",
 prompt_tokens: 6514, completion_tokens: 69, ms: 2202}
{event: mcp_call,    user: "1001", tool: "nlp_ask", ms: 2243}
{event: openai_model_denied, user: "1001", model: "gpt-5-turbo-max"}
→ answer: SELECT count() FROM players WHERE country IN ('VN','Vietnam','Việt Nam') → 4
```

Three rules the endpoint keeps, all testable:

- **Identity is the transport's.** A body carrying `platform_user_id: "1001"` while the token says
  `2002` is charged to `2002`. A caller that can name itself can name somebody else.
- **The whitelist is the guard.** `models.allow` names what may be reached; a role may only narrow
  it. A name outside it is refused with `404 model_not_found` **before** the provider is asked, and
  a role that may not use a model gets the same answer as one asking for a model that does not
  exist — otherwise the endpoint becomes a way to enumerate other roles' access.
- **The name passes through.** `model` is forwarded to the provider unchanged, so the whitelist
  lists provider model ids. There is no rename map: `gpt-4.1-mini` in the list means the provider
  will be asked for `gpt-4.1-mini`.

`stream: true` returns a valid SSE sequence — role delta, one content delta, finish, `[DONE]`. The
content arrives as **one chunk**, because the model service answers whole turns; a client sees the
same events in the same order, and this is stated rather than dressed up as token streaming.

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
