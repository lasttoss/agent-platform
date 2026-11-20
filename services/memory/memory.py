"""Memory for a platform with more than one person in it.

The question this service answers is not "where do we keep vectors" -- Postgres answers that in
one line. The questions that decide whether the thing is safe are these:

1. **Whose memory is this?** The answer comes from the scope object, which is built from an
   authenticated identity, and every statement filters on it. There is no `search(user_id=...)`
   on the API, because a `user_id` argument is a value a model can supply, and identity that a
   model can supply is not identity. `Memory.scope("")` raises.

2. **What happens when the same fact arrives twice?** It replaces. `(user_id, kind, key)` is
   unique, and `put` reports whether the row was inserted or replaced -- so "memory grows
   without bound" is a number someone can watch, not a fear.

3. **What keeps the table from growing anyway?** Four things, in order of how much they save:
   a row holds a fact (tens of bytes), not a conversation (hundreds of tokens); a tool result
   is stored as a *pointer* (path, sha256, size) and never inlined; every row may carry an
   expiry, and `sweep` deletes the ones that passed; and a body is capped at 2 KB by a CHECK
   constraint, so one large write fails loudly instead of quietly becoming a 200 MB row.

4. **What happens when a row expires?** Reads ignore it and `sweep` removes it. Expiry is
   enforced on the read path, not only by the cleaner, because the cleaner is a job and jobs
   are late.

What this does not do: it does not extract facts from conversations, and it does not summarise.
Those are policy decisions with a model in them, and they belong above this layer where they can
be measured against a fixed set of questions. This file is the store; it should stay boring.

Storage is Postgres with pgvector. There is no SQLite fallback: half the point of this service is
that two people's rows are separated by a `WHERE` clause that a test can prove, and a second
storage engine would be a second place for that clause to be forgotten.
"""
from __future__ import annotations

import hashlib
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Sequence

SCHEMA_PATH = Path(__file__).resolve().parent / "schema.sql"
BODY_LIMIT = 2048          # bytes of UTF-8; matches the CHECK constraint
DEFAULT_DIM = 1536


class MemoryError(RuntimeError):
    """Raised for a request this store refuses to guess about."""


def _hash(body: str) -> str:
    return hashlib.sha256(body.encode("utf-8")).hexdigest()


def _vector(values: Sequence[float]) -> str:
    """pgvector's text form. Sending a literal keeps the driver out of the type question."""
    return "[" + ",".join(f"{float(v):.6f}" for v in values) + "]"


@dataclass(frozen=True)
class Hit:
    kind: str
    key: str
    body: str
    score: float


class Scoped:
    """One person's memory. Construct it from an identity you already trust."""

    def __init__(self, store: "Memory", user_id: str) -> None:
        if not isinstance(user_id, str) or not user_id.strip():
            raise MemoryError("a scope needs an identity; it is not a request parameter")
        self._store = store
        self.user_id = user_id

    def put(
        self,
        kind: str,
        key: str,
        body: str,
        embedding: Sequence[float] | None = None,
        ttl_seconds: int | None = None,
    ) -> bool:
        """Write one fact. Returns True if it was new, False if it replaced an older one."""
        if not kind or not key:
            raise MemoryError("kind and key are how a fact is found again; both are required")
        size = len(body.encode("utf-8"))
        if size > BODY_LIMIT:
            raise MemoryError(
                f"body is {size} bytes, over the {BODY_LIMIT} byte cap: store a pointer instead"
            )
        vec = _vector(embedding) if embedding is not None else None
        with self._store._conn() as conn:          # type: ignore[attr-defined]
            row = conn.execute(
                """
                INSERT INTO memory (user_id, kind, key, body, hash, embedding, expires_at)
                VALUES (%s, %s, %s, %s, %s, %s::halfvec,
                        CASE WHEN %s::int IS NULL THEN NULL
                             ELSE now() + make_interval(secs => %s::int) END)
                ON CONFLICT (user_id, kind, key) DO UPDATE
                   SET body = EXCLUDED.body,
                       hash = EXCLUDED.hash,
                       embedding = EXCLUDED.embedding,
                       expires_at = EXCLUDED.expires_at,
                       updated_at = now()
                RETURNING (xmax = 0) AS inserted
                """,
                (self.user_id, kind, key, body, _hash(body), vec, ttl_seconds, ttl_seconds),
            ).fetchone()
        return bool(row["inserted"])

    def get(self, kind: str, key: str, run_id: str | None = None) -> str | None:
        with self._store._conn() as conn:          # type: ignore[attr-defined]
            row = conn.execute(
                """
                SELECT body FROM memory
                 WHERE user_id = %s AND kind = %s AND key = %s
                   AND (expires_at IS NULL OR expires_at > now())
                """,
                (self.user_id, kind, key),
            ).fetchone()
            self._store._record(conn, self.user_id, self.user_id, "get", 0 if row is None else 1, run_id)
        return row["body"] if row else None

    def search(
        self,
        embedding: Sequence[float],
        k: int = 5,
        kind: str | None = None,
        run_id: str | None = None,
    ) -> list[Hit]:
        vec = _vector(embedding)
        with self._store._conn() as conn:          # type: ignore[attr-defined]
            rows = conn.execute(
                """
                SELECT kind, key, body, 1 - (embedding <=> %s::halfvec) AS score
                  FROM memory
                 WHERE user_id = %s
                   AND embedding IS NOT NULL
                   AND (expires_at IS NULL OR expires_at > now())
                   AND (%s::text IS NULL OR kind = %s::text)
                 ORDER BY embedding <=> %s::halfvec
                 LIMIT %s
                """,
                (vec, self.user_id, kind, kind, vec, int(k)),
            ).fetchall()
            self._store._record(conn, self.user_id, self.user_id, "search", len(rows), run_id)
        return [Hit(r["kind"], r["key"], r["body"], float(r["score"])) for r in rows]

    def forget(self, kind: str | None = None, key: str | None = None) -> int:
        """Delete rows in this scope. Always scoped: there is no way to empty everyone's memory."""
        sql = "DELETE FROM memory WHERE user_id = %s"
        args: list[Any] = [self.user_id]
        if kind is not None:
            sql += " AND kind = %s"
            args.append(kind)
        if key is not None:
            sql += " AND key = %s"
            args.append(key)
        with self._store._conn() as conn:          # type: ignore[attr-defined]
            deleted = conn.execute(sql, tuple(args)).rowcount or 0
            # A deletion is the one write that cannot be undone by reading again, so it gets a
            # receipt even when nobody asked for one.
            self._store._record(conn, self.user_id, self.user_id, "forget", deleted)
            return deleted

    def count(self, kind: str | None = None) -> int:
        sql = "SELECT count(*) AS n FROM memory WHERE user_id = %s"
        args: list[Any] = [self.user_id]
        if kind is not None:
            sql += " AND kind = %s"
            args.append(kind)
        with self._store._conn() as conn:          # type: ignore[attr-defined]
            return int(conn.execute(sql, tuple(args)).fetchone()["n"])


class Memory:
    """Store-wide operations: schema, the cleaner, and the size report. No content access."""

    def __init__(self, dsn: str | None = None, dim: int = DEFAULT_DIM) -> None:
        self.dsn = dsn or os.environ.get("MEMORY_DSN", "")
        if not self.dsn:
            raise MemoryError("MEMORY_DSN is not set")
        self.dim = dim
        self._psycopg = None       # imported on first use: the module stays importable without it

    # -- plumbing ---------------------------------------------------------------------------
    def _conn(self):
        if self._psycopg is None:
            import psycopg
            from psycopg.rows import dict_row

            self._psycopg = (psycopg, dict_row)
        psycopg, dict_row = self._psycopg
        return psycopg.connect(self.dsn, row_factory=dict_row)

    def create_schema(self) -> None:
        sql = SCHEMA_PATH.read_text(encoding="utf-8").replace("{dim}", str(self.dim))
        with self._conn() as conn:
            conn.execute("CREATE EXTENSION IF NOT EXISTS vector")
            conn.execute(sql)

    # -- public API ------------------------------------------------------------------------
    def _record(self, conn, actor_id: str, subject_id: str, op: str, rows: int,
                run_id: str | None = None) -> None:
        """One row per read. Written in the same transaction as the read, so a read cannot happen
        without its receipt."""
        conn.execute(
            "INSERT INTO memory_event (actor_id, subject_id, op, rows, run_id)"
            " VALUES (%s, %s, %s, %s, %s)",
            (actor_id, subject_id, op, int(rows), run_id),
        )

    def scope(self, user_id: str) -> Scoped:
        return Scoped(self, user_id)

    def inspect(self, actor_id: str, subject_id: str) -> dict:
        """Support looking at somebody else's memory, and leaving a name behind.

        This is the read that has to exist -- "why did this person get that answer" is a real
        question -- and the one that has to be attributable. It returns shape, not content:
        per-kind counts and the newest timestamps, never a body."""
        if not actor_id or not subject_id:
            raise MemoryError("an inspection names both who looked and whose memory it was")
        with self._conn() as conn:
            rows = conn.execute(
                """
                SELECT kind, count(*) AS rows, max(updated_at) AS newest
                  FROM memory WHERE user_id = %s GROUP BY kind ORDER BY rows DESC
                """,
                (subject_id,),
            ).fetchall()
            self._record(conn, actor_id, subject_id, "inspect", sum(r["rows"] for r in rows))
        return {"subject_id": subject_id, "kinds": [dict(r) for r in rows]}

    def sweep(self) -> int:
        """Delete rows whose expiry has passed. Called by a job, never on the request path."""
        with self._conn() as conn:
            return conn.execute(
                "DELETE FROM memory WHERE expires_at IS NOT NULL AND expires_at < now()"
            ).rowcount or 0

    def stats(self) -> list[dict]:
        """Rows, body bytes and embedded rows per kind -- the answer to "how big is this"."""
        with self._conn() as conn:
            rows = conn.execute(
                """
                SELECT kind,
                       count(*)                        AS rows,
                       coalesce(sum(octet_length(body)), 0) AS body_bytes,
                       count(embedding)                AS embedded
                  FROM memory
                 GROUP BY kind
                 ORDER BY rows DESC
                """
            ).fetchall()
        return [dict(r) for r in rows]


def store_from_env() -> Memory:
    """The DSN and the vector width come from the deployment, not from a caller."""
    store = Memory(dim=int(os.environ.get("MEMORY_DIM", DEFAULT_DIM)))
    store.create_schema()
    return store
