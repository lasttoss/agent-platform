"""What may reach ClickHouse, and what is refused before a connection is opened.

This is deliberately a *guard*, not a parser. Writing a SQL parser to prove a string is a SELECT
is a losing race; ClickHouse already has the real boundary — a database user that may only read —
and this module exists so an obvious `DROP` never travels, so the error a caller sees names the
reason instead of echoing a database exception, and so a language model cannot turn a question
into a mutation.

The rules below are enforced on the text; the settings sent with every query (`readonly=1`, a
time limit, a row cap) are enforced by the server. Anything that needs a real parser — a `SELECT`
that calls a table function which writes — is out of scope here and stopped by the database user.
"""
from __future__ import annotations

import re
from typing import NamedTuple

MAX_SQL_CHARS = 8000

# Statements that change something. Checked as whole words outside string literals, so
# `SELECT 'DROP TABLE'` is a string and `DROP TABLE` is not.
FORBIDDEN_KEYWORDS = (
    "INSERT", "ALTER", "DROP", "CREATE", "TRUNCATE", "OPTIMIZE", "RENAME", "EXCHANGE",
    "GRANT", "REVOKE", "ATTACH", "DETACH", "SYSTEM", "DELETE", "UPDATE", "KILL", "SET",
    "BACKUP", "RESTORE", "USE", "SHOW", "DESCRIBE", "EXPLAIN",
)

# Anything that writes a file or changes how the answer is framed: we append our own FORMAT JSON,
# and a second one is either an error or, worse, an attempt to write to disk.
FORBIDDEN_PHRASES = (
    "INTO OUTFILE", "INTO DUMPFILE", "FORMAT", "SETTINGS", "PROFILE", "INFILE",
)

# Catalogues that must never be reachable through a question: they hold users, grants and quotas.
FORBIDDEN_PREFIXES = ("system.", "information_schema.", "default.")


class Verdict(NamedTuple):
    ok: bool
    reason: str
    sql: str


def _strip_comments(sql: str) -> str:
    """Remove -- and /* */ comments without breaking string literals.

    A guard that reads `SELECT 1 -- ; DROP TABLE x` as one statement is fine; one that reads
    `SELECT 'a -- b'` as a comment eats the rest of the query and passes a different string than
    the server will execute. This keeps quotes intact.
    """
    out = []
    i = 0
    quote = ""
    while i < len(sql):
        ch = sql[i]
        if quote:
            out.append(ch)
            if ch == "\\" and i + 1 < len(sql):
                out.append(sql[i + 1])
                i += 2
                continue
            if ch == quote:
                quote = ""
            i += 1
            continue
        if ch in ("'", '"', "`"):
            quote = ch
            out.append(ch)
            i += 1
            continue
        if sql.startswith("--", i):
            while i < len(sql) and sql[i] != "\n":
                i += 1
            out.append(" ")
            continue
        if sql.startswith("/*", i):
            end = sql.find("*/", i + 2)
            i = len(sql) if end < 0 else end + 2
            out.append(" ")
            continue
        out.append(ch)
        i += 1
    return "".join(out)


def _unquoted(sql: str) -> str:
    """The query with string literals blanked, for keyword matching only."""
    out = []
    quote = ""
    for ch in sql:
        if quote:
            if ch == quote:
                quote = ""
                out.append(" ")
            continue
        if ch in ("'", '"', "`"):
            quote = ch
            out.append(" ")
            continue
        out.append(ch)
    return "".join(out)


def check_readonly(sql: str) -> Verdict:
    """Refuse anything that is not a single read-only statement. Returns the SQL to run."""
    if not isinstance(sql, str):
        return Verdict(False, "sql must be a string", "")
    text = sql.strip()
    if not text:
        return Verdict(False, "empty sql", "")
    if len(text) > MAX_SQL_CHARS:
        return Verdict(False, f"sql longer than {MAX_SQL_CHARS} characters", "")

    cleaned = _strip_comments(text).strip()
    if cleaned.endswith(";"):
        cleaned = cleaned[:-1].strip()

    # Every textual rule reads the same quote-free copy, so `SELECT 'a;b'` is one statement with
    # data in it, not two statements.
    flat = re.sub(r"\s+", " ", _unquoted(cleaned)).strip()
    if not flat:
        return Verdict(False, "empty sql", "")
    if ";" in flat:
        return Verdict(False, "only one statement per call", "")

    words = flat.upper()
    # A catalogue is checked before a keyword, so `system.users` reads as "refused catalogue"
    # instead of the less useful "refused keyword: SYSTEM".
    lowered = flat.lower()
    for prefix in FORBIDDEN_PREFIXES:
        if prefix in lowered:
            return Verdict(False, f"refused catalogue: {prefix}", "")

    # Named keywords first: "DROP TABLE" should say "refused keyword: DROP", not "only SELECT",
    # because the first tells a caller what happened and the second sounds like a parser error.
    for kw in FORBIDDEN_KEYWORDS:
        if re.search(rf"\b{kw}\b", words):
            return Verdict(False, f"refused keyword: {kw}", "")

    first = re.match(r"([A-Za-z_]+)", words)
    if not first or first.group(1) not in ("SELECT", "WITH"):
        return Verdict(False, "only SELECT (including WITH … SELECT) may be sent", "")
    for phrase in FORBIDDEN_PHRASES:
        if re.search(rf"\b{re.escape(phrase)}\b", words):
            return Verdict(False, f"refused construct: {phrase}", "")
    return Verdict(True, "", cleaned)
