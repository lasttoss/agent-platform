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

## Still missing, in order of what would bite first

| Gap | Why it matters | Cost |
|---|---|---|
| Counters are eventually-consistent (5 s) | two replicas can overspend inside the window | a database round-trip per call, or Redis `INCRBY` |
| The bucket is per process | two replicas = two buckets per user | shared store, same shape as counters |
| No trace/span export | metrics say "slow", not "where" | OTel + collector, ~1 day |
| Long tools block a request | a 5-minute build must become job + poll | job store + SSE progress, ~2 days |
| Role store has no history | audit log has it, the UI does not | read `admin_role_change` back, ~half day |
