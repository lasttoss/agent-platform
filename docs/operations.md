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

## 3. Counters survive a restart

`-usage /state/usage.json` writes today's `requests`/`tokens` per user, debounced to at most two
writes per second, and loads them at boot. A restart at noon no longer hands everyone a second
allowance. A file written on a previous day is ignored on purpose: the quota is per day.

Limits, stated plainly: one process, one file. Two gateways sharing a file would overwrite each
other's counters — that needs Redis or a table, and this repository does not pretend to have it.

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
| Counters are per process | two replicas = two allowances | Redis/Postgres, ~1 day |
| No per-user rate limit | the daily cap is a cliff, not a slope | token bucket, ~half day |
| No trace/span export | metrics say "slow", not "where" | OTel + collector, ~1 day |
| Long tools block a request | a 5-minute build must become job + poll | job store + SSE progress, ~2 days |
| Role store has no history | audit log has it, the UI does not | read `admin_role_change` back, ~half day |
