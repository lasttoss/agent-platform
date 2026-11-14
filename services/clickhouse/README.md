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

## Limits, stated

- No `LIMIT` is injected into the caller's SQL; the row cap is enforced by `max_result_rows` and
  `result_overflow_mode = break`, and the response says `truncated: true` when it applied.
- No graph, no joins helper, no result formatting beyond JSON rows.
- The guard's catalogue check is a denylist of three names. A `db.table` reference to any other
  database is stopped by grants, not by the guard.
