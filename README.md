# agent-platform

[![test](https://github.com/lasttoss/agent-platform/actions/workflows/test.yml/badge.svg)](https://github.com/lasttoss/agent-platform/actions/workflows/test.yml)

One repo, four small services, one picture. The point of the repo is a single rule:

> **Identity comes from the transport. Authority comes from the gateway. Workers only work.**

Everything else here exists to make that rule runnable and testable on a laptop.

**Status: research notes, not production software.** Written to understand the problem and to be
explained out loud; no support, no SLA, no deployment story. 43 tests run with the Python standard
library alone (the OCR worker also needs poppler and Pillow). Everything that identifies a real
person or a real amount lives outside git, in `config/local.json`.

## Layout

```
services/gateway/     policy, identity, quota, audit, tool filtering   (Python now, Go next)
services/ocr/         tool worker: PDF text + image OCR                (no policy, no identity)
config/               policy.json, providers.json, consent.json         (shared contract)
docs/figures/         platform-map.png     — the whole system on one page
                      gateway-topology.png — gateway close-up
                      cache-layers.png     — the three cache layers and what each one saves
```

Each service keeps its own tests, Makefile targets and README, so it can be lifted out and
deployed alone. Nothing imports across service boundaries: gateway talks to workers over
HTTP/MCP only.

## Run

```bash
make test          # 19 tests: 12 gateway + 7 ocr, no network, no keys
make run-ocr       # 127.0.0.1:8099
make run-gateway   # 127.0.0.1:8088
```

End-to-end, with a real file:

```bash
TOOL_ROOT=/some/folder PORT=8099 python3 services/ocr/server.py &
OCR_SERVICE_URL=http://127.0.0.1:8099 PORT=8088 python3 -m gateway.server &
cd services/gateway && python3 cli.py --chat dm "ocr CV.pdf"
```

## The four decisions this repo encodes

| Decision | Where it lives | What breaks if you move it |
|---|---|---|
| The caller's identity comes from the signed platform event, never from text | gateway | a message can impersonate anyone |
| Tool visibility is filtered per `(user × chat)` **before** the model sees it | `gateway/policy.py` | prompt injection becomes privilege escalation |
| Workers hold no authority and no platform credentials | `services/ocr/` | one worker bug widens authority for everyone |
| Personal and third-party data leave the chat: DM only, aggregate in groups | `policy.json` + `server.py` | a salary or a candidate CV lands in a group |

## What is deliberately not here yet

- Real mail/Drive/Jira MCP servers. `config/providers.json` describes the shape (auth type,
  scopes, rate limits); the mock providers exist so consent rules are testable without
  credentials.
- A job queue. Long scans belong in one; the contract is described in `docs/figures/`
  and in each service README, and adding it does not change the boundary above.
- The Go port of the gateway. Same `policy.json`, same tests, different runtime.

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
