# agent-platform

[![test](https://github.com/lasttoss/agent-platform/actions/workflows/test.yml/badge.svg)](https://github.com/lasttoss/agent-platform/actions/workflows/test.yml)

One gateway, three services behind one protocol, one picture. The repo is built around one rule:

> **Identity comes from the transport. Authority comes from the gateway. Everything else is a
> service that only works.**

Concretely: **Go holds the gateway** (policy, consent, quota, cache, audit) and **everything a
caller can actually do is an MCP service** — including the model. The gateway contains no
extraction code, no mail client and no provider SDK. Adding a capability means starting a
service, not adding a branch.

**Status: research notes, not production software.** Written to understand the problem and to be
explained out loud; no support, no SLA, no deployment story. 91 tests: Go gateway 22, Python
reference gateway 43, OCR 21, model service 5. Everything that identifies a real person or a real
amount lives outside git, in `config/local.json`.

## Layout

```
services/gateway-go/  THE GATEWAY — policy, consent, quota, cache, audit, MCP client
  cmd/mcp-demo/       demo tool service: salary, documents, status   (MCP)
services/ocr/         tool service: PDF text + image OCR             (MCP, no policy, no identity)
services/model-mcp/   the model as a service: "which tool fits this sentence" (MCP, no tool access)
services/gateway/     the Python reference gateway + Telegram adapter (parity baseline)
config/               policy.json, consent.json, tools.json, mcp.json  (the contract)
scripts/parity.py     starts both gateways + services, diffs 10 decisions
docs/figures/         platform-map.png · gateway-topology.png · cache-layers.png
```

Each service keeps its own tests, Makefile targets and README, so it can be lifted out and
deployed alone. Nothing imports across service boundaries: gateway talks to workers over
HTTP/MCP only.

## Run

```bash
make test              # 91 tests, no network, no keys
make run-mcp-demo      # 127.0.0.1:8091  demo tools (MCP)
make run-ocr           # 127.0.0.1:8099  OCR service (MCP) + REST for the queue
make run-model         # 127.0.0.1:8097  model service (MCP, mock until MODEL_BASE_URL is set)
make run-gateway-go    # 127.0.0.1:8090  the gateway
make parity            # both gateways, same 10 requests, decisions diffed
```

End-to-end, with a real file — the gateway resolves the caller, calls one MCP tool, and the OCR
service does the reading:

```bash
curl -s -X POST 127.0.0.1:8090/v1/respond -H 'Content-Type: application/json' \
  -d '{"platform_user_id":"1001","chat_type":"dm","text":"đọc giúp tôi file samples/text.pdf"}'
# INVOICE 2026-11-A … Subtotal 1,234,000 VND - VAT 10 percent - Total 1,357,400 VND
```

## The four decisions this repo encodes

| Decision | Where it lives | What breaks if you move it |
|---|---|---|
| The caller's identity comes from the signed platform event, never from text | gateway | a message can impersonate anyone |
| A tool exists only if a running service **advertises** it *and* `tools.json` describes it | gateway `Registry` | an unreviewed tool on a service becomes a capability |
| Tool visibility is filtered per `(user × chat)` **before** the model sees it | `gateway/policy.py` | prompt injection becomes privilege escalation |
| Workers hold no authority and no platform credentials | `services/ocr/` | one worker bug widens authority for everyone |
| Personal and third-party data leave the chat: DM only, aggregate in groups | `policy.json` + `server.py` | a salary or a candidate CV lands in a group |

## What is deliberately not here yet

- Real mail/Drive/Jira MCP servers. `config/providers.json` describes the shape (auth type,
  scopes, rate limits); the mock providers exist so consent rules are testable without
  credentials.
- The job queue on the **gateway** side: the gateway still calls `ocr_extract` synchronously;
  the OCR service's own `/v1/jobs` queue is tested and ready for it.
- Auth between services beyond a bearer token per server (`config/mcp.json.token_env`) and
  loopback binding. Real deployments need mTLS or per-service credentials.

## Tool calling: the tool list is the permission

When a provider is configured the model chooses the tool, and it can only choose from the list
built for that caller and that chat:

```
visible_tools(rules, principal) ∩ consent   →   request `tools: [...]`   →   model returns tool_calls
                                                            │
                              name not in routable_tools ────┴──→ refused, never executed
```

`deploy_prod` is dropped by the loader because it is destructive, so it is **not in the request
at all** — the model cannot name it, and the caller cannot talk it into naming it. That is the
difference between filtering before the model and denying after the call; the execution guard
stays as the second line, because a model can still emit a name it was given by mistake.

Two tests hold this: one fake provider asks for `deploy_prod` (refused, nothing executed, and the
recorded request body proves it was never offered), one asks for `docs_search` (executed).

The trade-off is real: with a provider configured, even a request that matches no tool costs one
model turn, because the model is the thing choosing. That is why the cache sits **before** the
model turn, and why `free_chat` only decides whether the model's prose may be shown — not whether
the call happens.

## Model providers

The model layer speaks the OpenAI chat-completions shape, so a router such as LiteLLM drops in
without a code change:

```bash
MODEL_BASE_URL=http://127.0.0.1:4000/v1   # LiteLLM proxy: /v1/chat/completions
MODEL_API_KEY=<litellm master key>        # the gateway is the only holder, as with any provider
MODEL_NAME=claude-sonnet-4-5              # whatever alias the proxy exposes
```

Division of labour, so the two never blur:

| Concern | Owner |
|---|---|
| who may call which tool, in which chat | **gateway** (policy + consent + execution guard) |
| which provider answers, retries, fallbacks, spend per key | proxy (LiteLLM) |
| where the provider key lives | gateway only; the proxy holds its own master key |

`test_llm_proxy_base_url_with_path_prefix` pins the part that breaks in practice: a base URL with
a path prefix and a trailing slash must still produce `/v1/chat/completions`, with a bearer header.

## When no tool matches

The gateway does not fall back to ungrounded chat by default. A request that matches no tool
returns one fixed sentence and **never reaches a provider** — that path is where a demo starts
answering questions it has no data for, in the owner's name. Opting in is explicit, per chat:

```json
"chats": { "dm": { ..., "free_chat": true }, "group_public": { ..., "free_chat": false } }
```

Two tests pin both halves: with a counting fake endpoint, a no-tool question in a group makes
**0** model calls and costs **0** tokens, while the same question in a DM makes exactly **1**.
A policy that omits the key behaves like `false`.

## Three-layer cache (token cost)

See `docs/figures/cache-layers.png` for the same idea as a picture.

| Layer | What it matches | Key includes | Cost when it hits |
|---|---|---|---|
| **L1 exact** | same normalised request, same tool set, same policy version | text + sorted tools + policy version + chat type + **scope** | 0 model tokens, 1 request slot |
| **L2 semantic** | reworded question in the same scope (cosine ≥ 0.93) | scope + embedding | 0 model tokens, answer reused |
| **L3 prefix** | byte-identical prompt prefix at the provider | sorted tool list + policy version | static prefix billed once, then cached by the provider |

Scope is the security boundary: a personal answer is stored under `user:<id>` and is never
returned to another user. TTL is per tool — `salary_self` and `mail_list` are **not cached at
all** (`TTL = 0`), `status` lives 30 s, `docs_search` 1 h, `ocr_extract` 24 h.

```
POST /v1/respond → {"cache": {"layer": "exact", "saved_tokens": 0}}
GET  /stats/cache → {"l1_hit_rate": 0.5, "l2_hit_rate": 0.0, "tokens_saved_estimate": 0, "l3_prefix": "..."}
```

Run it twice and the second call never reaches a model:

```bash
python3 cli.py --chat dm "tài liệu runbook"     # miss, tool call
python3 cli.py --chat dm "tài liệu runbook"     # {"layer": "exact"}
```

Honest limit of L2 here: the shipped embedder is a hashed bag-of-words proxy, so it catches
reorderings with shared words, not true paraphrases. Swapping in a real embedding service
means implementing `embed()` against it; the store, threshold and scope rules do not change.

## Telegram adapter (live-tested)

`services/gateway/adapters/telegram_poll.py` turns an update into an authenticated gateway
request. It decides nothing about visibility — the gateway's answer already says
`delivered: chat | dm`, and the adapter obeys it. Its only security job: the DM target is always
the sender's own id, never a chat id taken from message text.

```bash
make run-gateway                     # :8088
BOT_TOKEN=... GATEWAY_URL=http://127.0.0.1:8088 make run-adapter
make demo-adapter                    # offline: dm, group→DM, unknown user, repeat
```

Behaviour that is tested, not assumed (`tests/test_adapter.py`, real HTTP to a fake Telegram API):

- a personal answer in a group is delivered **to the sender's DM**, and the group only sees
  "Mình gửi riêng cho bạn nhé."
- `423`/quota/403 come back as answers, not crashes: unknown user gets one refusal line
- the update offset is persisted, so a restart does not replay old messages
- `409 Conflict` (another process polling the same bot) exits with a clear message instead of looping
- 429 on `sendMessage` respects `retry_after`

The adapter is a thin shell on purpose: swapping Telegram for Discord means writing this file
again, not touching policy, cache or workers.

## Live run notes (2026-11-11)

Two things only a real bot could teach, both now fixed and covered by tests:

1. **Diacritics.** Vietnamese is typed without tones on a phone. `luong thang nay cua toi` fell
   through to "no tool for this" while `lương tháng này của tôi` worked. Intent matching and the
   cache key now both run on folded text, so `lương` and `luong` share one cache entry.
2. **`STATE_DIR` was documented but never read.** Pointing two servers at different state dirs
   silently left both on the same `cache.db` and the same daily quota. `make_server` now honours it.

Measured on the live bot (5 messages, `ms` = adapter total, gateway HTTP included):

```
gateway /v1/respond          13–38 ms
adapter total (incl. send)   966–1267 ms   → the rest is Telegram round-trip, not the gateway
cache                        exact hit on the 3rd and 5th message
```

## What is deliberately not in this repo

A gateway demo is easy to write with real data and very hard to un-publish. So:

| Not committed | Where the real value lives |
|---|---|
| real platform user ids | `config/local.json` (gitignored), merged at startup by `policy.apply_local` |
| real salary amounts | same file, under `salary`, loaded by `tools.load_salary_overrides` |
| `state/` — audit trail, cache, usage counters | local only: they contain ids and the text people asked |
| OCR sample documents | generated by `services/ocr/samples/make_samples.py` with a synthetic invoice |
| `__pycache__`, `*.db`, `*.log` | `.gitignore` |

`test_committed_policy_contains_no_real_person` fails the build if a committed template ever
grows an id longer than four digits or a salary literal that is not one of the two synthetic
fixtures. A guard is cheaper than remembering.

```bash
# what a template looks like, and what stays local
cat services/gateway/config/policy.json | python3 -m json.tool | head
cat config/local.json          # gitignored; not in git history
```

## License

MIT — see `LICENSE`. Without a license file the default is "all rights reserved": nobody may
legally copy, run or adapt this code, which defeats the point of publishing it as a work sample.

## Pre-commit guard

```bash
git config core.hooksPath .githooks     # once per clone
```

`.githooks/pre-commit` blocks a commit when the staged diff contains a bot token, an API key,
a private key, a hardcoded credential, an absolute home path, a platform id of 8+ digits used as
a key, a money literal that is not one of the counting-pattern fixtures, or any runtime file
(`state/`, `*.db`, `*.log`, `config/local.json`, generated samples).

Verified by staging a fake token, a real-looking id and a plausible amount: all three were
refused. The hook is a backstop, not the rule — the rule is that real values have one home,
`config/local.json`, and it is gitignored.

## Tests

```
make test     # 91 tests: 22 gateway-go, 43 Python reference gateway, 21 ocr, 5 model service
make parity   # starts both gateways + the MCP services, diffs the decisions: 10/10 equal
```
CI runs the same command on every push (`.github/workflows/test.yml`); the only system dependency
is poppler for the PDF text layer, and the OCR fixtures are generated, never committed.

## Measured, and what it does not prove

`scripts/bench.py` sends the same request to both gateways: 32 concurrent clients, 640 requests
each, on this laptop. Cache warm, no model attached.

```
gateway        rps    p50     p95     p99     max    errors
go            6298    4.2     7.6     9.7    11.8         0
python          68  463.6   698.1   928.4  1504.7         0
```

At one client the same request costs **0.2 ms** on Go and **12.3 ms** on Python. That single
number explains most of the table: the Python reference commits to sqlite on every request (a
durable cache), the Go gateway keeps an in-memory map behind a mutex. The rest is the global lock
that makes the shared sqlite connection safe.

**What this does not prove.** Nothing about Go versus Python as languages, and nothing about
event loops: both gateways are non-blocking at the socket layer. It measures two designs, one of
which pays fsync per request. It is also why the Go cache is documented as in-memory — that is a
trade (no cache after restart) made to keep the request path free of durability.

The measurement did find one real bug: sharing one `sqlite3` connection across request threads
without a lock dropped 381 of 640 connections with `InterfaceError: bad parameter or other API
misuse`. `CacheStore` now serialises its connection; the Python gateway answers 640/640 too.

## The MCP boundary

There is exactly one way for the gateway to reach a capability: JSON-RPC `tools/call` to a URL in
`config/mcp.json`. The gateway ships no tool implementation — `services/gateway-go/tools.go` holds
three structs and no logic.

Four rules make that boundary hold:

1. **Discovery is the allowlist.** At boot the gateway asks every service for `tools/list`. A tool
   that no running service advertises is invisible and uncallable, even if `policy.json` names it.
   `tools.json` then supplies the *policy* facts (personal, destructive, provider, scope, TTL) —
   because "who may use it" is not something a service gets to declare about itself.
2. **`config/mcp.json` narrows further.** A service may advertise ten tools; the config lists the
   ones this gateway is allowed to use. The unreviewed remainder never enters the registry.
3. **Identity travels in transport.** The gateway resolves the principal first, then calls the
   service with `X-Caller-Id`. `salary_self` takes **no user argument at all** — a model cannot
   name whose record it wants, and a test asserts the header, not a parameter.
4. **The model is a service too.** It answers "which tool fits this sentence", has no tool access,
   and returns nothing at all when unconfigured (`MODEL_BASE_URL` unset) instead of making up a
   reply. The gateway checks the returned name against `routable_tools` before anything runs.

```
chat ──▶ gateway ──▶ tools/list (boot)        ──▶ registry: name → service, schema
              ├────▶ tools/call docs_search   ──▶ demo service
              ├────▶ tools/call ocr_extract   ──▶ OCR service  (PDF engines live here)
              └────▶ tools/call chat_complete ──▶ model service (chooses a name, never acts)
```

Live check from the run above: `4 tools advertised by 3 service(s)`, and the OCR answer in the
terminal is the text out of a real PDF.

## Two gateways, one policy

`services/gateway/` (Python) stays as the reference implementation the parity script compares
against: same `policy.json`, same `consent.json`, same decisions. `scripts/parity.py` starts both
gateways plus the MCP services and diffs ten requests on status, delivery target, answer text,
denial code and cache layer — currently **10/10 identical**.

One difference is reported on every run instead of being hidden: the Go gateway lists only tools a
running service advertises, while the Python reference still carries a built-in registry for
services that do not exist yet. The Go behaviour is the intended one.
