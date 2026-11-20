# memory

One person's facts, stored where a `WHERE` clause can prove they are theirs.

The design questions this service answers, in the order they matter:

1. **Whose memory is this?** `Memory.scope(user_id)` is built from an identity you already trust.
   There is no `search(user_id=...)` on the API, because a `user_id` argument is a value a model
   can supply.
2. **What stops it growing?** A row is a fact, not a conversation; a tool result is stored as a
   pointer (`toolref.toolref`); every row may carry an expiry and reads ignore expired rows before
   the cleaner runs; a body over 2 KB is refused by a CHECK constraint, not truncated.
3. **What replaces what?** `(user_id, kind, key)` is unique. `put` returns `True` for a new fact
   and `False` for a replacement, so growth is a number, not a fear.
4. **Where are the bytes?** `Memory.stats()` reports rows, body bytes and embedded rows per kind.
5. **What happens when one person's working set gets large?** `Scoped.compact(keep=500)` moves the
   least recently used rows to `memory_archive` and leaves one `digest` row saying how many moved,
   how old, how new, and the first twenty keys. `Scoped.restore(kind, key)` brings one back.
   Nothing is deleted except by `Memory.purge_archive(days=180)`, which is called on purpose and
   reports how many rows it removed.

The digest is JSON, not prose, on purpose: a model summary of forty facts is a new claim with no
source, and the next turn only needs to know the facts exist and how to ask for one.

Not here on purpose: extraction and summarisation. Those use a model, so they belong above this
layer where they can be measured against a fixed set of questions.

## Running the tests

The isolation test needs a real Postgres with pgvector; without one the suite skips, and the skip
is visible.

```bash
docker run -d --rm --name mempg -e POSTGRES_PASSWORD=x -p 5544:5432 pgvector/pgvector:pg16
uv venv /tmp/memvenv && uv pip install --python /tmp/memvenv/bin/python "psycopg[binary]"
MEMORY_TEST_DSN=postgresql://postgres:x@127.0.0.1:5544/postgres \
  /tmp/memvenv/bin/python -m unittest discover -s services/memory/tests -v
```

Remove the `WHERE user_id = %s` from `get()` and `search()` and the first test fails with
`'vụ dâu tháng 11 giảm 12%' is not None` — that is the intended red.

## Reading the store

Reads leave a receipt: `memory_event` holds one row per read with `actor_id` next to
`subject_id`, no content. `Memory.inspect(actor, subject)` is the support path -- it returns
per-kind counts for somebody else and records itself as a cross-user read, which is the read that
needs a name later.

```bash
MEMORY_DSN=... python3 scripts/memory-report.py
```

Prints table sizes, rows and body bytes per kind, a one-year projection, reads in the last day,
cross-user reads in the last 30 days, and rows expiring this week. It exits non-zero when a kind
passes `MEMORY_ROW_BUDGET` (200k rows), the projection passes `MEMORY_BYTES_BUDGET` (4 GB), or a
cross-user read happened in the last day -- so it works as a cron check, not a report nobody reads.

## Configuration

| variable | meaning |
|---|---|
| `MEMORY_DSN` | Postgres connection string (the service refuses to start without one) |
| `MEMORY_DIM` | vector width, default 1536; must match the embedding model |
