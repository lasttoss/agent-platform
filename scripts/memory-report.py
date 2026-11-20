#!/usr/bin/env python3
"""What the memory store costs, where the bytes are, and who has been reading whom.

Run weekly, or before a deploy that touches the store. Every number here comes from the database
itself, because the alternative -- estimating -- is how a table quietly becomes the largest thing
on the disk.

    MEMORY_DSN=postgresql://... python3 scripts/memory-report.py

Exit code 1 when a guard trips (a kind over its row budget, or cross-user reads in the last day),
so this is usable as a cron check rather than a report somebody has to remember to read.
"""
from __future__ import annotations

import os
import sys

ROW_BUDGET = int(os.environ.get("MEMORY_ROW_BUDGET", "200000"))
BYTES_BUDGET = int(os.environ.get("MEMORY_BYTES_BUDGET", str(4 << 30)))


def human(n) -> str:
    n = float(n)
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if abs(n) < 1024 or unit == "TB":
            return f"{n:.1f}{unit}"
        n /= 1024
    return f"{n:.1f}TB"


def rows(conn, sql: str, args: tuple = ()) -> list[dict]:
    # The connection's row factory is dict_row, so a row is already a mapping. Zipping the
    # description against it would zip a dict's *keys* and quietly produce column names as values.
    with conn.cursor() as cur:
        cur.execute(sql, args)
        return [dict(r) for r in cur.fetchall()]


def main() -> int:
    dsn = os.environ.get("MEMORY_DSN", "").strip()
    if not dsn:
        print("MEMORY_DSN is not set", file=sys.stderr)
        return 2
    import psycopg
    from psycopg.rows import dict_row

    problems: list[str] = []
    with psycopg.connect(dsn, row_factory=dict_row) as conn:
        print("== tables ==")
        for r in rows(conn, """
            SELECT c.relname AS name,
                   pg_total_relation_size(c.oid) AS bytes,
                   (SELECT count(*) FROM pg_stat_user_tables s WHERE s.relid = c.oid) AS stat_rows
              FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
             WHERE n.nspname = 'public' AND c.relname IN ('memory', 'memory_event', 'memory_archive')
             ORDER BY bytes DESC"""):
            print(f"  {r['name']:<14} {human(r['bytes']):>9}")
        total = float(rows(conn, """
            SELECT coalesce(sum(pg_total_relation_size(c.oid)), 0) AS bytes
              FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
             WHERE n.nspname = 'public' AND c.relname IN ('memory', 'memory_event', 'memory_archive')
             """)[0]["bytes"])
        print(f"  {'total':<14} {human(total):>9}")

        print("\n== by kind ==")
        print(f"  {'kind':<14}{'rows':>9}{'body':>10}{'avg':>8}{'embed':>8}{'expired':>9}")
        kinds = rows(conn, """
            SELECT kind,
                   count(*)                                        AS n,
                   coalesce(sum(octet_length(body)), 0)            AS body_bytes,
                   coalesce(avg(octet_length(body)), 0)            AS avg_bytes,
                   count(embedding)                                AS embedded,
                   count(*) FILTER (WHERE expires_at < now())      AS expired
              FROM memory GROUP BY kind ORDER BY n DESC""")
        for k in kinds:
            print(f"  {k['kind']:<14}{k['n']:>9}{human(k['body_bytes']):>10}"
                  f"{k['avg_bytes']:>8.0f}{k['embedded']:>8}{k['expired']:>9}")
            if k["n"] > ROW_BUDGET:
                problems.append(f"kind {k['kind']} holds {k['n']} rows, over the {ROW_BUDGET} budget")

        print("\n== growth ==")
        grow = rows(conn, """
            SELECT count(*) FILTER (WHERE created_at > now() - interval '7 days')   AS week,
                   count(*)                                                          AS all_rows,
                   coalesce(sum(octet_length(body)) FILTER
                       (WHERE created_at > now() - interval '7 days'), 0)            AS week_bytes
              FROM memory""")[0]
        year_bytes = float(grow["week_bytes"]) / 7.0 * 365
        print(f"  last 7 days   {grow['week']:>8} rows   {human(grow['week_bytes']):>9}")
        print(f"  this rate     {human(year_bytes):>18} per year   (body bytes only, no indexes)")
        if total + year_bytes > BYTES_BUDGET:
            problems.append(f"the store would reach {human(total + year_bytes)} in a year, "
                            f"over the {human(BYTES_BUDGET)} budget")

        print("\n== archive (compacted out of the working set) ==")
        arch = rows(conn, """
            SELECT kind, count(*) AS n, coalesce(sum(octet_length(body)), 0) AS bytes
              FROM memory_archive GROUP BY kind ORDER BY n DESC""")
        if not arch:
            print("  empty")
        for r in arch:
            print(f"  {r['kind']:<14}{r['n']:>8} rows  {human(r['bytes']):>10}")
        if arch:
            purge = rows(conn, """
                SELECT count(*) AS n FROM memory_archive
                 WHERE archived_at < now() - interval '180 days'""")[0]["n"]
            print(f"  older than 180 days: {purge} rows ready for purge_archive()")

        print("\n== reads, last 24 hours ==")
        for r in rows(conn, """
            SELECT op, count(*) AS calls, coalesce(sum(rows), 0) AS returned
              FROM memory_event WHERE at > now() - interval '1 day'
             GROUP BY op ORDER BY calls DESC"""):
            print(f"  {r['op']:<10}{r['calls']:>8} calls   {r['returned']:>8} rows returned")

        print("\n== cross-user reads, last 30 days ==")
        cross = rows(conn, """
            SELECT actor_id, subject_id, op, count(*) AS calls, max(at) AS last_at
              FROM memory_event
             WHERE actor_id <> subject_id AND at > now() - interval '30 days'
             GROUP BY actor_id, subject_id, op ORDER BY calls DESC LIMIT 10""")
        if not cross:
            print("  none")
        for r in cross:
            print(f"  {r['actor_id']} -> {r['subject_id']}  {r['op']:<8}{r['calls']:>5} calls"
                  f"   last {r['last_at']:%Y-%m-%d %H:%M}")
        cross_24h = rows(conn, """
            SELECT count(*) AS n FROM memory_event
             WHERE actor_id <> subject_id AND at > now() - interval '1 day'""")[0]["n"]
        if cross_24h:
            problems.append(f"{cross_24h} cross-user reads in the last 24 hours")

        print("\n== expiring within 7 days ==")
        soon = rows(conn, """
            SELECT kind, count(*) AS n FROM memory
             WHERE expires_at IS NOT NULL AND expires_at < now() + interval '7 days'
             GROUP BY kind ORDER BY n DESC""")
        if not soon:
            print("  none")
        for r in soon:
            print(f"  {r['kind']:<14}{r['n']:>8}")

    if problems:
        print("\n== problems ==")
        for p in problems:
            print(f"  ! {p}")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
