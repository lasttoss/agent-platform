# ClickHouse, behind MCP, with a question in front of it

Three tools:

| tool | who may call it | what it does |
|---|---|---|
| `ch_schema` | dev, owner | tables and columns in the configured database |
| `ch_query` | owner only | one read-only `SELECT`, rows as JSON, plus the SQL used |
| `nlp_ask` | dev, owner | a question in words → one `SELECT` from the model service → checked → run |

Raw SQL is an owner capability on purpose: everybody else asks in words, and the words go through
the same guard the owner's SQL does.

## Two boundaries, and which one is real

1. **`sqlguard.py`** — text rules. One statement, must start with `SELECT`/`WITH`, no mutation or
   DDL keywords, no `INTO OUTFILE`, no `FORMAT`, no `SETTINGS`, no `system.`/`information_schema.`/
   `default.`. The keyword check reads a copy with string literals blanked, so `SELECT 'DROP TABLE'`
   is data and `SELECT 1 -- x` + `; DROP TABLE events` is still two statements.
2. **The database user** — `app_readonly`, created in `deploy/compose/clickhouse/init.sql` with
   `SETTINGS readonly = 2` and `GRANT SELECT ON analytics.*`. This is the real boundary, measured:

```
INSERT INTO analytics.events …  → Code 497 ACCESS_DENIED (no INSERT grant)
DROP TABLE analytics.events     → Code 497 ACCESS_DENIED (no DROP grant)
```

The rules exist so an obvious mutation is refused with a reason instead of a stack trace, and so a
model cannot turn a question into a write. They are not a parser and do not pretend to be one.

`readonly = 2`, not `1`: the service sends `max_execution_time` and `max_result_rows` with every
query, and a `readonly = 1` user refuses *any* settings change (`Code 164`). Sending `readonly=1`
as a per-query setting has the same problem — it is a change to a readonly setting.

## What `nlp_ask` actually does

1. read the schema (cached for the tool's TTL by the gateway, not here);
2. call the model service — an MCP service like any other, `chat_complete`, no vendor SDK;
3. refuse anything that is not one read-only statement;
4. run it with a row cap and a time limit;
5. return the SQL **with** the rows: an answer nobody can check is a rumour.

When the model service is unconfigured it answers with empty content and this returns
`model_returned_no_sql` instead of inventing a query.

### Measured against a real model

```
MODEL_BASE_URL=https://api.commandcode.ai/provider/v1 \
MODEL_NAME=deepseek/deepseek-v4.1-flash \
MODEL_API_KEY=… docker compose -f deploy/compose/docker-compose.yml up -d model-mcp
```

Then, through the gateway (`nlp_ask`, owner token), a real DeepSeek V4.1 Flash behind the real
`model-mcp` and a real ClickHouse:

```
Q: Sự kiện nào xuất hiện nhiều nhất và trung bình mất bao nhiêu ms?
   SELECT event, count() AS cnt, avg(duration_ms) AS avg_ms FROM events
   GROUP BY event ORDER BY cnt DESC LIMIT 1          → 1 row, 3 ms
   level_start · cnt 12 · avg_ms 1195.8

Q: Có bao nhiêu người chơi đến từ VN?
   SELECT count() FROM players WHERE country = 'VN'  → 4          (5 ms)

Q: Xoá toàn bộ dữ liệu trong bảng events
   → no_table_can_answer_this: the model found no table for the question
   (the model declined to write a DELETE at all; asked to translate a DELETE verbatim,
    it still answered NONE, so the guard was not the one that stopped it — see the unit
    test that feeds it a DELETE and asserts nothing reaches the database)
```

What this proves: the whole chain works with a real model, the schema reaches the prompt, the SQL
is returned for inspection, and a destructive question produces no write. What it does not prove:
that translation is always correct — a wrong-but-valid SELECT would be executed, which is why the
SQL is printed next to the rows.

## Aggregate materialized views: what breaks, measured

A schema description that only lists column names lets a model write `SELECT sum(duration_ms)
FROM events`. That is right until the raw table is trimmed and the aggregate states are the only
history left, and then it is confidently wrong. `ch_schema` therefore describes the engine, points
a view at its target, and names the combinator **per column**:

```
events_daily (AggregatingMergeTree)
  events    AggregateFunction(count)          → countMerge(events)
  total_ms  AggregateFunction(sum, UInt32)    → sumMerge(total_ms)
  avg_ms    AggregateFunction(avg, UInt32)    → avgMerge(avg_ms)

events_daily_mv (MaterializedView → events_daily)
# A materialized view keeps the pre-aggregated history; the raw table may be trimmed by TTL.
```

Why per column: `sumMerge(events)` on a count state fails as *"Aggregate function sum requires
single argument"* — measured, and a confusing way to learn that the column is not a number.

### Two writers, same schema, same database

`scripts/bench_nlp_mv.py`: 7 questions (all requiring `-Merge` combinators), 3 runs per cell,
**raw trimmed to 1.4M rows while the view still holds 5M**, DeepSeek V4.1 Flash for both,
so the only difference is who calls the model.

```
writer          correct      median    p90    used the aggregated table
our nlp_ask     20/21 (95%)  2536 ms   3899 ms   20/21
ClickHouse      17/21 (80%)  2047 ms   3217 ms   21/21
  aiGenerate
```
- **Accuracy** is the same order; the misses were both writers producing an aggregate of an
  aggregate (`sum(countMerge(events))`) that ClickHouse rejects — a model mistake, not a path
  mistake. The sample is 7 questions: this is a smoke-sized benchmark, not a claim about quality.
- **Latency** is dominated by the model call (1.5–5 s), not by the hop: in-database is ~20 %
  faster in this sample and that is inside the noise of a network model.
- **Both** now use the materialized view, which is the point: the fix that mattered was the schema
  text, not the caller.

Cost of reading the raw table instead, measured on 46.4M rows:

```
SELECT event, avg(duration_ms) FROM events GROUP BY event        → 46.4M rows · 43–50 ms
SELECT event, avgMerge(avg_ms) FROM events_daily GROUP BY event   →        192 rows · 1.7–2.3 ms
```

At 46.4M rows the wall-clock gap is ~22x; the row gap is ~240000x. Small data hides this, which is
why the benchmark trims the raw table instead of trusting speed alone.

## Limits, stated

- No `LIMIT` is injected into the caller's SQL; the row cap is enforced by `max_result_rows` and
  `result_overflow_mode = break`, and the response says `truncated: true` when it applied.
- No graph, no joins helper, no result formatting beyond JSON rows.
- The guard's catalogue check is a denylist of three names. A `db.table` reference to any other
  database is stopped by grants, not by the guard.
