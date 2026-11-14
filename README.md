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
services/gateway-go/  THE GATEWAY AND PROXY — policy, roles, consent, quota, cache, audit, MCP client
  rbac.go             one reason code per hidden tool; backs GET /v1/whoami
  cmd/mcp-demo/       demo tool service: salary, documents, status   (MCP)
services/ocr/         tool service: PDF text + image OCR, submit + read (MCP, no policy, no identity)
services/userdir/     who exists, and whether they may act             (MCP, Postgres or SQLite)
services/audit-sink/  one audit view for several gateways               (HTTP, SQLite)
services/model-mcp/   the model as a service: "which tool fits this sentence" (MCP, no tool access)
services/mcp-ts-example/  a tool service in TypeScript: the boundary is the protocol (MCP)
services/console/     the operator console (Vite + React); nginx serves it and proxies the API
services/gateway/     Python REFERENCE gateway + Telegram adapter (parity oracle, not the deployment)
config/               policy.json, consent.json, tools.json, mcp.json  (the contract)
scripts/parity.py     starts both gateways + services, diffs 10 decisions
docs/rbac.md          the gates, in order, and the reason code each one produces
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

The whole platform in containers — gateway, four MCP services, Postgres, the audit sink and the
console:

```bash
docker compose -f deploy/compose/docker-compose.yml up --build
open http://127.0.0.1:8088       # console: paste a token whose role has admin: true
bash scripts/compose_smoke.sh    # or let the script assert every claim below
```

End-to-end, with a real file — the gateway resolves the caller, calls one MCP tool, and the OCR
service does the reading:

```bash
curl -s -X POST 127.0.0.1:8090/v1/respond -H 'Content-Type: application/json' \
  -d '{"platform_user_id":"1001","chat_type":"dm","text":"ocr samples/text.pdf"}'
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

## ClickHouse service

`services/clickhouse/` answers four tools: `ch_schema` (tables, engines, combinator per aggregate
column), `ch_explain` (`EXPLAIN ESTIMATE`, nothing runs), `ch_query` (one read-only SELECT with
`{name:Type}` parameters, owner only) and `nlp_ask` (a question in words → one SELECT from the
model service → checked → run, with the SQL returned next to the rows and one repair round when
the SQL is refused). Every query travels through `sqlguard.py`
and then through a ClickHouse user that may only read. See `services/clickhouse/README.md`.

`nlp_ask` needs a model: point the model service at any OpenAI-compatible endpoint (the guard and
the read-only database user stay in the path whoever the provider is).

A real agent harness is the honest test of "MCP all the way down": `deploy/dsh/dsh-platform.patch.yml`
composes dsh with the gateway as its only tool server, and `docs/dsh-client.md` records what that
run proved, including the two failures it exposed.

```
MODEL_BASE_URL=https://api.openai.com/v1 MODEL_NAME=gpt-4.1-mini \
MODEL_API_KEY=… docker compose -f deploy/compose/docker-compose.yml up -d model-mcp

docker compose -f deploy/compose/docker-compose.yml up -d --build clickhouse clickhouse-mcp
curl -s localhost:8090/mcp -H 'Authorization: Bearer dsh-demo-owner-token' -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ch_query","arguments":{"sql":"SELECT event, count() AS n FROM events GROUP BY event"}}}'
```

## Long-running tools

Mark a tool `"long_running": true` in `config/tools.json` and the call returns
`{"job_id": "...", "state": "running", "poll": "/v1/jobs/<id>"}` immediately; poll that path with
the same bearer token. Jobs are per user (another user's id is a 404), capped at four running per
user, audited, and swept an hour after they finish. They live in one process's memory: no queue,
no retry, no durability. See `docs/operations.md` 2d.

## Tracing

Off by default. `./gateway -otlp http://127.0.0.1:4318` exports one trace per request, with a
`mcp.tool` child span carrying the tool, the role, the cache outcome and the upstream duration.
No span attribute contains a user id — the trace id comes back in `X-Trace-Id` instead. See
`docs/operations.md` 2c.

## Operating it

`docs/operations.md` covers the five things beyond "answers correctly": argument validation before
any work, per-tool deadlines, counters that survive a restart, `/metrics` with no user ids in it,
and a shutdown that finishes in-flight calls. The short version:

```bash
curl -s 127.0.0.1:8090/metrics | grep gateway_mcp_cache_misses_total
```

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
python3 cli.py --chat dm "docs runbook"     # miss, tool call
python3 cli.py --chat dm "docs runbook"     # {"layer": "exact"}
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
  "Sending that to you privately."
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
   This is also the only Vietnamese left in the repository, on purpose: those strings are *inputs*
   that prove the folding works. Every reply, comment, document, test name and figure caption is
   English, so a reviewer who does not read Vietnamese can still audit all of it.
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
make test     # 140 tests: 54 gateway-go, 45 Python reference gateway, 21 ocr, 8 userdir,
              #            7 audit-sink, 5 model service
make parity   # starts both gateways + the MCP services, diffs the decisions (10 + 60 generated)
PARITY_FUZZ=300 python3 scripts/parity.py   # same, 300 generated cases
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

Footprint, same laptop (RSS from `ps`, binary from `ls`):

```
go gateway            binary 8.7 MB   idle 8.6 MB RSS   14.9 MB after 3200 requests at 32 clients
python reference      —             idle 25.6 MB RSS
node 22 bare http     —             idle 44.8 MB RSS   (an empty server returning {"ok":true})
```

The Node row is a *bare* server; the gateway rows do policy, cache and audit. It is in the table
because "typed JS is the light choice for a proxy" is a claim worth testing rather than
repeating — the runtime is the weight.

### Goroutines are cheap; the queue is not

"Coroutines are light, so concurrency is free" is true about the scheduler and false about the
system. Measured against a downstream that behaves like a real tool service — one worker, 200 ms
per call, so 5 calls/s — with 20 questions from 20 clients at once:

```
max_in_flight     wall    p50      p95      p99      outcomes
none              4.01s   2007ms   3815ms   3815ms   20 × 200 (everyone waits 4 s for a 200 ms call)
4                 0.80s      0.9ms  602ms    602ms   4 × 200, 16 × 503 in ~1 ms
8                 1.60s      1.4ms 1405ms   1405ms   8 × 200, 12 × 503 in ~1 ms
```

Goroutines did not remove the queue; they let the gateway accept it. The waiting moved from
inside the service to inside the caller, and the caller had no way to know. `max_in_flight` lives
in `config/tools.json`, the shed answer is `503 service_busy`, and the audit log records
`decision: "shed"` — the same shape as a deliberate refusal, because it is one.

`0` means unlimited, which is a decision to prefer accepting over answering. The defaults here
are 8 for `salary_self`/`docs_search`, 4 for `ocr_extract` (a PDF engine is the most expensive
thing in the repo) and 16 for `status`.

### Shared state, if there is ever a second replica

Today quota, cache and audit are per-process: two replicas mean two quotas and two caches. That
is the first thing that breaks, and it breaks in correctness, not in speed. Measured on this
laptop, Postgres 16 in Docker, loopback, no pooler, 10 000 keys:

```
operation                              conc=1            conc=16            conc=64
SELECT by primary key                  p50 0.13 ms       p50 0.45 ms        p50 1.00 ms  p99 29 ms
UPsert, synchronous_commit=on          p50 2.10 ms       p50 3.75 ms        —
UPsert, synchronous_commit=off         p50 0.16 ms       p50 0.45 ms        —
server-side index scan                                   Execution 0.052 ms
```

Reads are cheap enough for a shared cache; **the commit is the cost**, and turning it off is
legitimate for derived data (losing a second of cache costs one extra tool call) and not
legitimate for a quota or a consent grant. The ordering that follows:

| State | Where it belongs once there are 2+ gateways | Why |
|---|---|---|
| quota counters | durable, atomic (`UPDATE … RETURNING`) or Redis `INCR` | a lost increment is a promise broken |
| cache L1/L2 | Redis, or Postgres with `synchronous_commit=off` | derived data; loss is a cache miss, not a wrong answer |
| policy + consent | Postgres, read-mostly, cached in memory by version | must agree across replicas |
| audit | append-only table or object storage, written async | durability matters, latency in the request path does not |
| long jobs (many PDFs) | RabbitMQ / SQS with an idempotency key + DLQ | see below |

Queues are not a replacement for a concurrency limit: a queue only helps work whose answer is
*not needed now* (`202` + poll, which the OCR service already does). On the synchronous path a
queue converts "slow" into "timeout", which is why the semaphore above exists.

**What this does not prove.** Nothing about Go versus Python as languages, and nothing about
event loops: both gateways are non-blocking at the socket layer. It measures two designs, one of
which pays fsync per request. It is also why the Go cache is documented as in-memory — that is a
trade (no cache after restart) made to keep the request path free of durability.

The measurement did find one real bug: sharing one `sqlite3` connection across request threads
without a lock dropped 381 of 640 connections with `InterfaceError: bad parameter or other API
misuse`. `CacheStore` now serialises its connection; the Python gateway answers 640/640 too.

## Roles: what RBAC decides, and what it cannot

`policy.json` now has a `roles` table, and it is not decoration. Before this change `role` was
parsed, attached to the principal, and **never consulted**: a user marked `"guest"` had exactly
the owner's powers. A field that looks like a control but is not one is worse than no field.

```
"roles": {
  "owner": {"tools": ["*"]},
  "dev":   {"tools": ["salary_self", "docs_search", "status", "ocr_extract"]},
  "guest": {"tools": ["docs_search", "status"]}
}
```

Two rules make it safe (full reference: [docs/rbac.md](docs/rbac.md)):

1. **A role narrows, never widens.** The effective set is
   `chat allowlist ∩ role grant ∩ consent ∩ advertised ∩ not destructive`. `"*"` means "as much as
   this chat already allows" — never more, so a new role cannot become a backdoor.
2. **An undefined role fails the load.** A user pointing at a role that does not exist is a
   configuration error, not a person with default access.

What a role still cannot decide, and why the other gates stay:

| Question | Answered by | Why not RBAC |
|---|---|---|
| may this person use `salary_self`? | role + chat + consent | yes, RBAC fits |
| may the answer be posted in this group? | `personal` flag + `personal_data` | a role is about *who*, delivery is about *what the data is* |
| may the gateway read this person's Drive? | per-user consent grant with a scope | delegated consent is per-user and revocable; a role cannot hold it |
| is the tool available right now? | MCP discovery | runtime fact, not a permission |

Every hidden tool has exactly one reason code, and `GET /v1/whoami?user=&chat=` prints it:

```
docs_search      visible  delivery=chat
salary_self      hidden   blocked_by=not_in_chat
drive_search     hidden   blocked_by=service_not_advertising
```

That is the difference between "the model is broken" and "this chat is not allowed to see salary",
and it is a query instead of a code read.

At two or more replicas the roles table moves to Postgres (read-mostly, cached in memory by
policy version) — the same rule as the rest of the shared state above.

## Reachability is re-checked, not assumed

Discovery used to be a boot-time snapshot, which made deploy order a thing a human had to
remember: start the gateway first and a service that came up a minute later stayed invisible
until a restart. Every 30 seconds (`-refresh`) the gateway now probes each service and re-reads
its tool list, in that order:

| What the probe says | What the gateway does | Why |
|---|---|---|
| `/health` 200 | re-read `tools/list` | new tools appear, removed tools disappear |
| `/health` 503/timeout | drop that service's tools immediately | offering a dead tool turns a fast 503 into a slow timeout |
| `/health` 404 | treat as "no health endpoint", keep going | a plain MCP server is not an outage |
| `tools/list` fails, health was 200 | keep the previous list | one bad read must not empty the catalogue |

The refresh reports only changes (`+[docs_search] -[status]`), because a log line per tick that
says "nothing happened" is how real changes stop being noticed.

## Extraction is a queue, not a chat turn

`ocr_extract` submits and returns a job id; `ocr_result` reads it. The gateway remembers that id
per `(user, chat)`, so the follow-up question is just "result" — an opaque id is the gateway's
state, not something a person or a model should have to carry:

```
you:   read samples/text.pdf
bot:   Extraction queued as 8f2c1a9b3d4e. Ask "result" when you want it.
you:   result
bot:   Extraction finished: 420 characters over 2 page(s).
```

Three details that make this honest rather than decorative:

- a job id is never cached (`ttl = 0`): caching it would serve a stale id tomorrow;
- one person's pending job is not another person's — the map is keyed by `(user, chat)`;
- the blocking path still exists (`wait_seconds`), because a caller that genuinely wants to wait
  should be able to say so.

## Who exists is a Postgres question

`services/userdir` answers one question that no other service is allowed to answer: **is this
person still allowed to act?** The gateway asks it on the same 30-second timer and refuses a
disabled account before policy, before the model, before the cache:

```
{"denied":{"code":"user_disabled","reason":"this account is disabled in the user directory"}}
```

```
{"denied":{"code":"user_not_in_directory","reason":"the user directory does not know this caller"}}
```

The two codes are different on purpose. `user_disabled` means somebody switched this person off.
`user_not_in_directory` means the two systems disagree about whether the person exists — usually
a forgotten offboarding on one side, occasionally a typo on the other.

`make check-users` prints that disagreement and exits non-zero:

```
{"checked":3,"missing_in_directory":["3003"],"unknown_in_caller":["2002"],
 "inactive":["2002"],"match":false}
```

The comparison is symmetrical, because both directions are broken: a person policy.json
provisions and the directory does not know, and a directory row nobody removed.

Three rules keep the directory from becoming a silent widening:

1. **the store is reported, never inferred** — every answer carries `store: postgres` or
   `store: sqlite`, so a deployment cannot mistake the test store for the real one;
2. **no silent fallback** — `USERDIR_DSN` set but `psycopg` missing stops the service instead of
   quietly serving a local file, because that would mean "disabled in Postgres" had no effect;
3. **a stale copy still refuses** — if the directory stops answering, the last copy stays in
   force and `/v1/whoami` says `stale: true`. Reverting to policy.json would re-enable everyone
   somebody disabled on purpose.

What this does not prove: the Postgres path is exercised in this repo through `psql` (schema,
upsert, disable), not through the Python driver, which is not installed on the machine this was
built on. The SQL is verified; the binding is not.

## The whole platform in containers

```bash
make compose-up        # build + start every process and Postgres
bash scripts/compose_smoke.sh          # assert every claim below, end to end
make compose-down      # stop and remove volumes
```

Eight containers, no shared runtime: `gateway` (Go), `mcp-demo` (Go tool service), `ocr` (Python +
poppler), `model-mcp` (Python), `userdir` (Python + psycopg → `postgres`), `audit-sink` (Python),
`mcp-ts-example` (Node), `postgres` (the user directory only).

What the smoke script proves, in order:

```
directory source is the service, not policy.json   (Postgres reached, 30s refresh picked it up)
dm status 200, answer from the Go tool service      (gateway → mcp-demo over the container network)
public group: "I have no tool for that."            (policy, not a service decision)
disabled user: 403 user_disabled                    (Postgres says no)
unknown user: 403 user_not_in_directory             (the two systems disagree)
wiki_search answered                                (a tool served by the TypeScript process)
extraction queued, then read back                   (async job across two containers)
audit sink aggregated every event                   (18 events, 3 users, one view)
```

Three things this exercise taught, all of them invisible on one host:

1. **`depends_on: service_started` is not readiness.** The gateway booted, found `userdir`
   unreachable, fell back to policy.json, and only picked the directory up on the next 30-second
   tick. That is the timer working as designed — but it is also why the smoke script waits for
   `directory.source == "userdir"` instead of trusting "container started".
2. **a healthcheck inside a container proves nothing about reachability.** `userdir` answered
   `127.0.0.1:8093/health` while being bound to loopback and therefore invisible to every other
   container, with a green health status the whole time. Binding is now one convention (`HOST`,
   loopback by default) across every Python service, and the smoke script checks from outside.
3. **a port already in use leaves a container created but unattached.** `docker compose up` then
   gave `restart: unless-stopped` a container with no network at all, which presents as a DNS
   failure (`failed to resolve host 'postgres'`) rather than as a port conflict. `--force-recreate`
   fixes it; the README says so because it cost a debugging session.

## One audit view, several gateways

Each gateway keeps writing its own JSONL file — that file is the durable record, because an audit
trail that depends on a network hop is a trail with a hole in it. `services/audit-sink` is the
aggregate: every line is also shipped there, asynchronously, and a full buffer drops the delivery
while keeping the line:

```
$ AUDIT_SINK_URL=http://127.0.0.1:8096/v1/audit ./gateway-go
$ curl -s http://127.0.0.1:8096/v1/report?day=2026-11-12
{"day":"2026-11-12","events":4,"decisions":{"tool_unavailable":4},
 "tools":{"status":2,"docs_search":2},"users":{"1001":4},"shed_rate":0.0,"denied_rate":0.0}
```

Two design points that came out of writing it:

- **the event id is the identity, not the content.** The first version hashed the line minus its
  timestamp, so four distinct events collapsed into two. Now every line carries an `event_id`
  (`crypto/rand`), a retry keeps its id, and two requests inside the same second stay two events.
- **rates, not counts.** "3 shed" means nothing without the denominator, so the report divides by
  the day's total. `denied_rate` counts both `denied` and `tool_not_allowed`: a person refused a
  tool is a denial whoever refused them.

## Operator commands

```
gateway-go -check-users          # compare the directory with policy.json, exit 1 on mismatch
gateway-go -set-active 2002:false # disable a person in Postgres, through the same MCP tool
gateway-go -refresh 30s          # health + tool re-read interval, 0 disables
gateway-go -audit-http URL       # ship lines to the collector as well
```

`-set-active` is deliberately a flag and not a chat tool: `user_set_active` is marked destructive
in `tools.json`, so no model and no chat can reach it. The gateway calls it on behalf of an
operator and writes an audit line that says `"by":"operator"`.

## Postgres, actually run

The directory was verified against a real `postgres:16-alpine` through a containerised service
(`USERDIR_DSN=...`, `psycopg` installed in the image), not only against SQLite:

```
enable 1001 → {'store': 'postgres', 'id': '1001', 'active': True}
disable 2002 → {'store': 'postgres', 'id': '2002', 'active': False}
psql check    → 1001|t   2002|f
consistency   → {"missing_in_directory":["3003"],"inactive":["2002"],"match":false}
```

That run found two bugs no SQLite test could have found, which is the argument for running it:

1. `SCHEMA.replace("INTEGER", "BOOLEAN")` produced `active BOOLEAN NOT NULL DEFAULT 1` — valid
   SQLite, rejected by Postgres. Two schemas are now written out instead of generated by string
   surgery.
2. the service bound to `127.0.0.1` and was therefore unreachable through a published container
   port. Loopback stays the default (a directory should not be on the network by accident) and
   `USERDIR_HOST` makes opening it an explicit act.

## The gateway is a proxy; the services choose their own language

`services/gateway-go` is the only component that terminates a client request, and it owns no
domain logic: no PDF parsing, no salary lookup, no model call. It resolves identity, applies roles
and consent, enforces quota, writes the audit line, and **proxies** the call to whichever MCP
server advertises the tool. Everything else is a service that could have been written in anything.

| Component | Language | Why that language | What it must not do |
|---|---|---|---|
| `services/gateway-go` | Go, stdlib only | a proxy is I/O and policy; 8.7 MB binary, no runtime, a semaphore instead of a queue | parse a PDF, call a model, know what a salary is |
| `services/ocr` | Python | one example of an MCP *service*: the PDF text layer and OCR engines are better in Python | decide who may call it, or see an identity |
| `services/model-mcp` | Python | the model is a service too, reachable only as an MCP tool | touch another tool, or hold a key the gateway needs |
| `services/userdir` | Python (SQLite, Postgres via `USERDIR_DSN`) | the authority on *who exists*; a directory is a database question, not a code question | decide what a person may do — that is the gateway's job |
| `services/mcp-ts-example` | TypeScript, zero dependencies | proof that a new capability costs 2 config lines and 0 gateway lines | anything the other services do not do |

Adding a capability in a language that does not exist in this repo yet is a row in
`config/mcp.json`, a row in `config/tools.json`, and a server that speaks `tools/list` and
`tools/call`. The gateway is not recompiled and its tests do not change:

```
language is not a trust boundary — the protocol, the allowlist, the policy facts and the
identity header are. That is the whole claim, and TypeScript is here to falsify it.
```

The honest cost of the choice is deployment, not architecture: an empty Node process idles at
44.8 MB against the Go gateway's 8.6 MB (measured, see *Footprint*). Python appears twice because
it is the right tool for OCR and for the model client, not because the platform is Python.

`services/gateway/` (Python) is a **reference implementation**, kept as the parity oracle: it
re-implements the same decisions independently so a disagreement between the two shows up as a
failing case instead of a bug report. It is not the deployment path, and the README says so where
the two differ.

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

The same gateway already talks to services in three languages — Go (demo tools), Python (OCR,
model) and a TypeScript example (`services/mcp-ts-example/`). Adding the TypeScript tool cost
**two config edits and zero lines of gateway code**; the generic `hits` renderer covers the chat
text, and anything more specific stays in the gateway on purpose.

## Two gateways, one policy

`services/gateway/` (Python) stays as the reference implementation the parity script compares
against: same `policy.json`, same `consent.json`, same decisions. `scripts/parity.py` starts both
gateways plus the MCP services and diffs status, delivery target, answer text, denial code and
cache layer: **10 handwritten cases and 300 generated ones** (`PARITY_FUZZ=300`), all identical
on the answer. Three real divergences came out of the generated set, none out of the ten I wrote:

| Found by | Divergence | Fix |
|---|---|---|
| generated | a configured tool with its service down answered "no tool for this" | both gateways answer `503 service_unavailable` |
| generated | each model turn also consumed a request slot, halving the daily quota | requests and tokens counted separately |
| generated | a status answer was served for a documents question from the semantic cache | the tool is part of the semantic cache identity |

That is the honest answer to "parity is all we need": parity written by hand tests the cases its
author remembered. Parity generated from the domain vocabulary found three bugs in one afternoon.

One difference is reported on every run instead of being hidden: the Go gateway lists only tools a
running service advertises, while the Python reference still carries a built-in registry for
services that do not exist yet. The Go behaviour is the intended one.
