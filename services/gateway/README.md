# agent-gateway

A minimal policy gateway that sits between a chat client (Telegram, Discord) and an
agent runtime. It exists to prove one thing with code instead of prose:

> **Identity comes from the transport. Authority comes from policy. Personal data
> never lands in a shared chat.**

Standard library only. No FastAPI, no LiteLLM, no provider key required to run the tests.

## Run

```bash
make test    # 8 policy tests, no network, no key
make run     # gateway on 127.0.0.1:8088
make demo    # four end-to-end calls showing the same question in three chat types
```

`make demo` output, verbatim:

| chat type | what the caller sees in the chat | delivered |
|---|---|---|
| `dm` | `Lương tháng này của bạn là 12.345.000 VND.` | chat |
| `group_internal` | `Mình gửi riêng cho bạn nhé.` + the number by DM | dm |
| `group_public` | `Mình chưa có công cụ cho việc này.` | chat |
| unknown user | `403 {"denied": {"code": "unknown_user"}}` | — |

## API

```
POST /v1/respond
{ "platform_user_id": "1001", "chat_type": "dm", "text": "lương tháng này của tôi" }
```

`chat_type` is one of `dm`, `group_internal`, `group_public`. In a real deployment the
transport adapter fills `platform_user_id` from the signed platform event, never from
the message text.

```
GET /health
GET /stats?platform_user_id=1001
```

## Policy file

`config/policy.json` has two tool lists per chat type, and the difference is the point:

- `tools` — exposed to the model. A tool absent from this list is not merely refused;
  the model is never told it exists.
- `private_tools` — executable for this caller, but the result must be delivered
  privately. Only tools marked `personal` may sit here; anything else fails closed.

```json
"group_internal": {
  "tools": ["docs_search", "status"],
  "personal_data": false,
  "private_tools": ["salary_self"]
}
```

A malformed policy raises at load time. There is no code path where a missing field
degrades into "allow".

## Enforced rules

1. Unknown caller → `403`. No guest fallback unless `allow_unknown_users` is true.
2. Tools are filtered per `(user, chat)` **before** the model sees them.
3. Tools marked `destructive` can never be listed in a chat config; the loader skips
   them, so `deploy_prod` is unreachable from chat by construction.
4. Personal tools have no "whose data" argument. The subject is the authenticated
   caller, so asking for someone else's salary returns your own.
5. Personal answer in a non-personal chat → computed, delivered by DM, and the chat
   reply carries no number.
6. Daily quota per user (requests + tokens) → `429`.
7. Every decision is appended to `state/audit.log` as JSONL.

## Model is optional

Without `MODEL_BASE_URL` the gateway answers from tool results only (mock mode). With it,
the answer is polished by any OpenAI-compatible endpoint, and the system prompt forbids
inventing numbers:

```bash
export MODEL_BASE_URL=https://api.example.com/v1
export MODEL_API_KEY=...
export MODEL_NAME=gpt-4o-mini
make run
```

The model receives facts, never credentials. Tool results are the source of truth; if the
model fails or is unavailable, the deterministic answer is returned instead.

## Not implemented (intentionally)

- Transport authentication. This service trusts `platform_user_id` because the adapter in
  front of it has already verified the signed platform event. Do not expose port 8088 to
  the internet.
- Approval buttons for destructive actions, idempotency keys for retries, per-IP rate
  limiting, and multi-tenant sandboxing. Each is a separate layer, and each is cheaper to
  add when the policy boundary above is already correct.

## Layout

```
config/policy.json     users, chat types, tool lists, quota
gateway/policy.py      the only module that decides what is allowed (pure, no I/O)
gateway/tools.py       demo tool registry; declares personal / destructive
gateway/model.py       OpenAI-compatible adapter, mock by default
gateway/server.py      HTTP surface, audit log, quota, intent routing
cli.py                 terminal client
tests/test_gateway.py  8 policy tests
```
