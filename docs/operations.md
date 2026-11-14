# Operating the gateway

Five things a platform has to do beyond answering correctly, and what this repository actually
does about each one. Numbers here were measured on one machine; conditions are stated with them.

## 1. Arguments are checked before anything is spent

The gateway validates tool arguments against the `inputSchema` the service advertises, before the
quota, the cache and the in-flight slot are touched.

```
{"q": 42}         → invalid arguments: arguments.q wants a string, got number
{}                → invalid arguments: missing required argument "q"
{"q":"a","x":1}   → invalid arguments: unknown argument "x"     (when additionalProperties:false)
```

The enforced subset is stated in `schema.go`: `type`, `properties`, `required`, `enum`,
`items`, `additionalProperties`. Anything else in a schema is ignored, and the validator reports
which keywords it ignored rather than implying completeness. Invalid calls never reach the
service, and are audited as `mcp_invalid_args`.

## 2. A tool gets the deadline it deserves

`tools.json` sets `timeout_s` per tool (default 60):

```json
"ocr_extract": {"ttl": 0, "timeout_s": 300}
```

A PDF that takes minutes and a search that takes seconds cannot share one number. Measured: a tool
declaring `0.05s` against a service sleeping 400 ms fails at ~50 ms, not at 60 s.

## 2b. A burst is refused by a bucket, not by the cap

`policy.json` sets `quota.requests_per_minute` and `quota.burst` (both apply to `/mcp` and
`/v1/respond`, because a limit on one door is a suggestion). Per user, refilled continuously,
idle buckets swept.

```
25 calls in parallel, 1/min with burst 20 → 20 served, 5 refused
   {"code": -32006, "message": "rate limited: retry in 3s"}  + Retry-After: 3
```

The wait is computed from the bucket's actual deficit, so "retry in 3s" is a number the bucket
will honour. Refusals are audited as `mcp_rate_limited` and counted in
`gateway_mcp_rate_limited_total`.

## 3. Counters survive a restart, and add up across replicas

`-usage /state/usage.json` writes today's `requests`/`tokens` per user, debounced to at most two
writes per second, and loads them at boot. A restart at noon no longer hands everyone a second
allowance. A file written on a previous day is ignored on purpose: the quota is per day.

Two backends:

| Backend | Flag | Scope |
|---|---|---|
| JSON file | `-usage /state/usage.json` | one process; a second replica would overwrite it |
| Postgres | `-usage-dsn ...` (or `USAGE_DSN`) | replicas add up: each process writes only the delta it has not reported |

The delta rule is what makes two replicas sum instead of clobber:

```
effective(user) = db(user) + (local(user) - reported(user))
```

Measured: two replicas spending 2 and 1 requests end at 3 in the database; a third replica that
loads and immediately flushes writes nothing (this was a real double count, found live: a restart
took 20 requests to 40). Reload every 5 s, so between writes two processes can each spend a little
of the same allowance. That window is the honest cost of not putting a database round-trip in
front of every tool call.

## 4. Metrics, and no user ids in them

`GET /metrics` serves Prometheus text: cache hits/misses, coalesced calls, quota and saturation
refusals, SSE replies, active tokens, calls per tool, policy generation.

```
gateway_mcp_cache_misses_total 12
gateway_mcp_tool_calls_total{tool="wiki_search"} 4
gateway_roles_generation 3
```

Tool names and roles appear; user ids do not. A metric label is a place personal data leaks, and
cardinality grows with every employee.

## 5. Shutdown that finishes what it started

`SIGTERM`/`SIGINT` stops accepting, waits up to 15 s for in-flight calls, then writes the counters
and exits with `gateway stopped cleanly`. Compose sends `SIGTERM` on `down`, so a deploy no longer
cuts a tool call in half and forgets the quota it just spent.

## 2d. A slow tool becomes a job instead of a timeout

`tools.json` marks a tool `long_running: true`; the call returns an id instead of waiting:

```
tools.json   "ocr_extract": {"ttl": 0, "timeout_s": 300, "long_running": true}

call         {"job_id":"337188de...","state":"running","tool":"jira_get_issue","poll":"/v1/jobs/337188de..."}
             → returned in 6 ms, while the tool itself took 31 ms
poll         GET /v1/jobs/<id>   + the same bearer token as /mcp
             {"state":"done","ms":31,"result":{...}}
```

Rules that make it safe rather than merely asynchronous:

- **The id is a capability.** Another user's job and a made-up id both answer `404` (measured);
  no token answers `401`. A `403` would confirm the id exists.
- **The request is charged when accepted, the tokens when the answer exists.** A job that
  produced 4 MB of JSON did spend it.
- **Running jobs are capped per user (4) and refused, not queued** — a burst of jobs is a burst
  of goroutines nobody asked for. Refusals are `-32007`, counted as `gateway_jobs_refused_total`.
- **The in-flight slot belongs to the work, not to the caller**, so a build that outlives the
  request still counts against `max_in_flight`.
- Audited as `mcp_job_started` / `mcp_job_done` / `mcp_job_failed`, with the job id and duration.

What this is not, stated plainly: there is no queue, no worker pool, no retry, and no durable
store. Jobs live in one process's memory and are swept an hour after they finish. Kill the
gateway and the running jobs are gone — which is why nothing in this repository marks a
long-running tool by default.

## 2c. A slow answer can be attributed to one service

Tracing is off by default: with no `-otlp` (or `OTLP_ENDPOINT`) there is no exporter, and no span
is recorded. Turning it on exports OTLP/HTTP to a collector:

```
./gateway -otlp http://127.0.0.1:4318
tracing on — exporting spans to http://127.0.0.1:4318
```

One trace per request, with a child span per tool call:

```
POST /mcp                      (server span, name = method + route)
└── mcp.tool  mcp.tool=wiki_search gateway.role=owner gateway.chat=dm
              gateway.cache=miss|hit|coalesced|bypass  gateway.leader=true
              gateway.upstream_ms=31  gateway.rpc_error=<code>
```

Measured on one machine: a `curl` to `/mcp` returns `X-Trace-Id:
d069dd8e456bcb6f612e8878da780b74`, and that trace id appears **inside** the exported protobuf
after the batch flush (666 bytes on the wire). A caller that sends `traceparent` keeps its trace:
the reply header echoes the incoming trace id, so the adapter and the gateway are one trace, not
two.

Two rules, the same ones the metrics follow:

- **No user id on a span.** A trace is exported to a third party by definition. The role and the
  chat type are attributes; the identity comes back as `X-Trace-Id` so a user report can be joined
  to the span without the span carrying the user.
- **Spans are named by route**, not path: `/admin/users/1001/role` and `/admin/users/2002/role` are
  `GET /admin/users/{id}/role` — one line in a dashboard, not one per user.

Nothing traces the service side yet: the gateway injects `traceparent` into the upstream request,
and a service that does not read it starts its own trace. The four test cases in `tracing_test.go`
cover the gateway half, including that no attribute contains the user id.

## Parity harness notes

`make parity` compares the Go gateway with the Python reference on 70 requests. Three rules keep
it meaningful instead of green:

- **The bucket is off for the Go side.** The harness fires sixty requests at a few users in under
  a second; comparing "Python has no bucket" is true and uninteresting.
- **The OCR service runs for both sides.** It starts before the gateways, because the Go gateway
  discovers services once at boot — a service still binding its port does not exist for the run.
- **A known divergence is counted, not hidden.** The Python reference answers a private-only OCR
  tool inside a group chat; the Go gateway sends the same answer by DM, which is what
  `policy.json` says. It is printed as `known` and counted separately, so it cannot mask a new
  difference.

Latest run: `10/10 handwritten · 60/60 generated identical · 3 same-answer cache notes ·
3 known divergences`.

## Still missing, in order of what would bite first

| Gap | Why it matters | Cost |
|---|---|---|
| Counters are eventually-consistent (5 s) | two replicas can overspend inside the window | a database round-trip per call, or Redis `INCRBY` |
| The bucket is per process | two replicas = two buckets per user | shared store, same shape as counters |
| Nothing traces the MCP services | the gateway span says which tool and how long, not what the service did inside | OTel SDK in each service, ~1 day |
| Jobs are in-memory and single-process | a restart loses them; two replicas cannot poll each other's jobs | durable job store, ~1–2 days |
| Jobs have no progress | a 5-minute job shows "running" and then "done" | SSE progress from the service, ~1 day |
| Role store has no history | audit log has it, the UI does not | read `admin_role_change` back, ~half day |
