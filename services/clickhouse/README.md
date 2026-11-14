# ClickHouse, behind MCP, with a question in front of it

Four tools:

| tool | who may call it | what it does |
|---|---|---|
| `ch_schema` | dev, owner | tables, engines, which view feeds which table, and the combinator each aggregate column needs |
| `ch_explain` | dev, owner | `EXPLAIN ESTIMATE`: the rows and parts a query would read, without running it |
| `ch_query` | owner only | one read-only `SELECT` with optional `{name:Type}` parameters, rows as JSON, plus the SQL used |
| `nlp_ask` | dev, owner | a question in words → one `SELECT` from the model service → checked → run, with one repair round |

Raw SQL is an owner capability on purpose: everybody else asks in words, and the words go through
the same guard the owner's SQL does.

## Two boundaries, and which one is real

1. **`sqlguard.py`** — text rules. One statement, must start with `SELECT`/`WITH`, no mutation or
   DDL keywords, no `INTO OUTFILE`, no `FORMAT`, no `SETTINGS`, no `system.`/`information_schema.`/
   `default.`. The keyword check reads a copy with string literals blanked, so `SELECT 'DROP TABLE'`
   is data and `SELECT 1 -- x` + `; DROP TABLE events` is still two statements.
   On top of the text rules, two semantic checks refuse SQL that is well-formed and still cannot be
   right: an aggregate over an aggregate state (`sum(countMerge(events))`), and the right combinator
   on the wrong state (`sumMerge(events)` where `events` is a count state — the column types come
   from the database, cached for a minute, and the refusal names the combinator to use instead).
   Both refusals are what earns `nlp_ask` its single repair round.
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
MODEL_BASE_URL=https://api.openai.com/v1 MODEL_NAME=gpt-4.1-mini \
MODEL_API_KEY=… docker compose -f deploy/compose/docker-compose.yml up -d model-mcp
```

Then, through the gateway (`nlp_ask`, owner token), a real model behind the real `model-mcp` and a
real ClickHouse:

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

`scripts/bench_nlp_mv.py` with `gen50.py`-style generated cases: **50 questions**, questions and
ground truth written by the same template (the model never sees the truth), 5M raw rows over 7
days, an 84-row view, DeepSeek V4.1 Flash for both, each writer measured in its own sequential
pass so a provider hiccup does not silently become an accuracy number.

```
writer                correct   median    p90    first-attempt repairs
nlp_ask (this service) 49/50    2506 ms  6379 ms   15
ClickHouse aiGenerate  37/50    2259 ms  3483 ms    0
```

- **Every one of the 13 in-database misses was the same mistake**: `sum(countMerge(events))` — an
  aggregate over an aggregate state. This service refuses that text locally, tells the model which
  combinator to use, and retries once: 15 of the 50 questions needed that repair, and 14 of them
  came back correct. A path that can only forward the error to the user has no way to recover.
- The single service miss was an empty completion (`model_unavailable: timed out`), not a wrong
  answer — worth keeping in the number, because a benchmark that hides provider failures
  understates how often the answer is missing.
- **Latency is the model, not the path**: both sit at ~2.3–2.5 s median. The in-database p90 is
  better here (3.5 s vs 6.4 s) because a repair round is a second call.
- **Tokens, service only**, as the provider reported them: 473 prompt / 191 completion per
  question on a 20-question sample. ClickHouse does not expose usage per `aiGenerate` call, so the
  in-database path's token cost is **not measured** — not "cheaper", unknown.
- Measured one writer after the other. An interleaved pass (both writers per question) produced
  empty completions from the provider and a fake 17/50 for this service; the speed of a benchmark
  can change its answer, which is why the passes are separate.

### Why the view exists, before trimming anything

Same question ("events per day"), raw 5M rows vs the 84-row view: `read_rows` 5,000,000 against
84. At 46.4M rows the wall clock separates as well: 43–50 ms against 1.7–2.3 ms.

Point is not "the view is faster". The point is that the raw table may be **trimmed** —
`ALTER TABLE … DELETE WHERE ts < …` is what a retention policy does — and then only the view
still knows the history. That state is where a model's table choice stops being a speed
difference and starts being a wrong answer:

```
SELECT sum(duration_ms) FROM events        → 21.06 billion   (truth 75.24 billion)
SELECT count() FROM events                 → 1,400,000       (truth 5,000,000)
SELECT avg(duration_ms) … WHERE platform=… → 60198.66        (truth 60194.30)
```

Both writers made those mistakes until `ch_schema` described the engines, each view's target and
the combinator per column. After that, both read the view: the fix that mattered was the schema
text, not who calls the model.

## Limits, stated

- No `LIMIT` is injected into the caller's SQL; the row cap is enforced by `max_result_rows` and
  `result_overflow_mode = break`, and the response says `truncated: true` when it applied.
- No graph, no joins helper, no result formatting beyond JSON rows.
- The guard's catalogue check is a denylist of three names. A `db.table` reference to any other
  database is stopped by grants, not by the guard.
