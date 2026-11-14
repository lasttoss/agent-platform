"""ClickHouse behind MCP, and a question in plain language in front of it.

Three tools, chosen because they answer three different questions:

- `ch_query` runs one read-only SELECT. The SQL comes from a caller that is allowed to write SQL.
- `ch_schema` says what exists, so a person (or a model) does not have to guess table names.
- `nlp_ask` takes a question, asks the model service for one SELECT, refuses anything that is not
  a read-only statement, then runs it. The SQL that was attempted is returned with the answer:
  an answer nobody can check is a rumour.

Nothing here decides who may call what. The gateway does. This service only decides what may
reach ClickHouse, and it decides that twice: text rules in `sqlguard.py`, and a database user that
may only read. The user is the real boundary — measured, an INSERT and a DROP from this service's
credentials fail with ACCESS_DENIED — while the rules exist so an obvious mutation is refused with
a reason instead of a stack trace, and so a model cannot turn a question into a write.

The per-query settings are deliberately not `readonly=1`: that is a *change* to a readonly
setting, and ClickHouse refuses it (Code 164). The user is created with `readonly=2` plus
SELECT-only grants, which permits the row cap and the time limit while refusing writes.
"""
from __future__ import annotations

import json
import os
import time
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import sqlguard

CH_URL = os.environ.get("CLICKHOUSE_URL", "http://clickhouse:8123").rstrip("/")
CH_USER = os.environ.get("CLICKHOUSE_USER", "app")
CH_PASSWORD = os.environ.get("CLICKHOUSE_PASSWORD", "")
CH_DATABASE = os.environ.get("CLICKHOUSE_DB", "analytics")
CH_TIMEOUT = float(os.environ.get("CH_TIMEOUT_S", "15"))
MODEL_MCP_URL = os.environ.get("MODEL_MCP_URL", "http://127.0.0.1:8097/mcp")
DEFAULT_LIMIT = int(os.environ.get("CH_ROW_LIMIT", "200"))

QUERY = {
    "name": "ch_query",
    "description": "Run one read-only SELECT against ClickHouse. Returns rows as JSON, plus the SQL used.",
    "inputSchema": {
        "type": "object",
        "properties": {
            "sql": {"type": "string", "description": "A single SELECT (or WITH … SELECT). No mutations."},
            "limit": {"type": "integer", "description": f"Row cap for this query (default {DEFAULT_LIMIT}, max 1000)."},
        },
        "required": ["sql"],
        "additionalProperties": False,
    },
}

SCHEMA = {
    "name": "ch_schema",
    "description": "List tables and their columns in the configured database.",
    "inputSchema": {
        "type": "object",
        "properties": {"table": {"type": "string", "description": "Optional table name to describe."}},
        "additionalProperties": False,
    },
}

ASK = {
    "name": "nlp_ask",
    "description": ("Answer a question in plain language by asking the model for one SELECT, "
                    "checking it is read-only, and running it. Returns the SQL with the rows."),
    "inputSchema": {
        "type": "object",
        "properties": {
            "question": {"type": "string", "description": "The question, in any language."},
            "limit": {"type": "integer", "description": f"Row cap (default {DEFAULT_LIMIT}, max 1000)."},
        },
        "required": ["question"],
        "additionalProperties": False,
    },
}

TOOLS = [QUERY, SCHEMA, ASK]
SCHEMA_PROMPT = """You translate a question into ClickHouse SQL.

Rules, all mandatory:
- Output exactly one SELECT statement (WITH … SELECT is allowed). No prose, no markdown fences.
- Never write, never DDL, never a second statement, never `system.` or other catalogues.
- Use only the tables and columns below; if the question cannot be answered with them, output
  the single word NONE.

Schema:
{schema}
"""


def _ch(query: str, params: dict[str, str], timeout: float) -> str:
    """One HTTP call to ClickHouse. Credentials go in headers, never in a URL that gets logged."""
    url = f"{CH_URL}/?{urllib.parse.urlencode(params)}"
    req = urllib.request.Request(url, data=query.encode(),
                                 headers={"X-ClickHouse-User": CH_USER,
                                          "X-ClickHouse-Key": CH_PASSWORD})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return resp.read().decode()


def _ch_error(exc: Exception) -> str:
    """The first line of a ClickHouse error, with the query text trimmed off: enough to fix a
    query, short enough to read on a phone, and free of the credentials in the URL."""
    body = ""
    if hasattr(exc, "read"):
        try:
            body = exc.read().decode("utf-8", "replace")
        except Exception:
            body = ""
    first = (body or str(exc)).strip().splitlines()
    return first[0][:300] if first else "clickhouse error"


def schema_text(table: str = "") -> str:
    """Tables, engines and — where it matters — which table is a pre-aggregated view.

    A model that only sees column names will happily `SELECT sum(duration_ms) FROM events`. That
    is correct until the raw table is trimmed and the aggregate states are the only history left,
    at which point it is confidently wrong. So the description names the engine, points a
    materialized view at its target, and says out loud that AggregateFunction columns need their
    `-Merge` combinator.
    """
    where = "WHERE database = currentDatabase()"
    if table:
        where += " AND table = " + "'" + table.replace("'", "''") + "'"
    cols = json.loads(_ch(
        "SELECT table, name, type FROM system.columns " + where +
        " ORDER BY table, position FORMAT JSON",
        {"database": CH_DATABASE, "max_execution_time": "5"}, 10)).get("data") or []
    tables = json.loads(_ch(
        "SELECT name, engine, create_table_query FROM system.tables " + where +
        " ORDER BY name FORMAT JSON",
        {"database": CH_DATABASE, "max_execution_time": "5"}, 10)).get("data") or []
    if not cols and not tables:
        return f"(no tables in {CH_DATABASE})" if not table else f"(no table named {table})"

    by_table: dict[str, list[tuple[str, str]]] = {}
    for row in cols:
        by_table.setdefault(row["table"], []).append((row["name"], row["type"]))
    engines = {t["name"]: (t["engine"], t.get("create_table_query") or "") for t in tables}

    blocks = []
    for name in sorted(set(by_table) | set(engines)):
        engine, ddl = engines.get(name, ("", ""))
        header = name
        if engine and engine != "MergeTree":
            header += f" ({engine}"
            target = _mv_target(ddl)
            if target:
                header += f" → {target}"
            header += ")"
        elif engine:
            header += f" ({engine})"
        body = list(by_table.get(name, []))
        block = [header]
        width = max((len(c) for c, _ in body), default=0)
        for col, typ in body:
            line = f"  {col.ljust(width)}  {typ}"
            merge = _merge_hint(col, typ)
            if merge:
                line += f"   → {merge}"
            block.append(line)
        if any(t.startswith("AggregateFunction(") for _, t in body):
            block.append("  # each aggregate state has exactly one combinator, shown per column: "
                         "a count state needs countMerge, not sumMerge")
        blocks.append("\n".join(block))
    out = "\n\n".join(blocks)
    if any("MaterializedView" in e for e, _ in engines.values()):
        out += ("\n\n# A materialized view keeps the pre-aggregated history; the raw table may be "
                "trimmed by TTL. Prefer the aggregated table for questions about history.")
    return out


def _merge_hint(column: str, typ: str) -> str:
    """The combinator for one aggregate column, spelled out: `sumMerge(events)` on a count state
    is a type error ClickHouse reports as "Aggregate function sum requires single argument", which
    is a confusing way to learn that the column does not hold a number."""
    if not typ.startswith("AggregateFunction("):
        return ""
    inner = typ[len("AggregateFunction("):].rstrip(")")
    fn = inner.split(",")[0].strip().lower()
    if not fn:
        return ""
    return f"{fn}Merge({column})"


def _mv_target(ddl: str) -> str:
    """The table a materialized view writes into (`TO db.table`), for the one-line header."""
    marker = " TO "
    i = ddl.upper().find(marker)
    if i < 0:
        return ""
    rest = ddl[i + len(marker):].strip().split()[0]
    return rest.split(".")[-1].strip("`")


def run_sql(sql: str, limit: int = DEFAULT_LIMIT, timeout: float | None = None) -> dict:
    """Validate, then run. Never called with text that has not passed `sqlguard`."""
    verdict = sqlguard.check_readonly(sql)
    if not verdict.ok:
        return {"is_error": True, "error": "sql_rejected: " + verdict.reason, "sql": (sql or "")[:400]}
    cap = max(1, min(int(limit or DEFAULT_LIMIT), 1000))
    started = time.time()
    try:
        raw = _ch(verdict.sql, {
            "database": CH_DATABASE,
            "max_execution_time": str(int(timeout or CH_TIMEOUT)),
            "max_result_rows": str(cap),
            "result_overflow_mode": "break",
            "default_format": "JSON",
        }, (timeout or CH_TIMEOUT) + 5)
    except Exception as exc:  # a database failure is reported with its own words
        return {"is_error": True, "error": "clickhouse: " + _ch_error(exc),
                "sql": verdict.sql, "ms": int((time.time() - started) * 1000)}
    doc = json.loads(raw or "{}")
    rows = doc.get("data") or []
    return {"is_error": False, "sql": verdict.sql, "rows": rows, "row_count": len(rows),
            "rows_before_limit_at_least": doc.get("rows_before_limit_at_least"),
            "truncated": len(rows) >= cap, "ms": int((time.time() - started) * 1000)}


def _model_sql(question: str) -> tuple[str, str]:
    """Ask the model service for one SELECT. Returns (sql, why-not) — exactly one is non-empty.

    The model service is an MCP service like any other, so this is a `tools/call`, not an SDK.
    When it is unconfigured it answers with empty content, and this returns a reason instead of
    inventing a query: a fabricated SELECT that happens to run is worse than no answer.
    """
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                       "params": {"name": "chat_complete", "arguments": {
                           "system": SCHEMA_PROMPT.format(schema=schema_text()),
                           "user": question[:2000]}}}).encode()
    req = urllib.request.Request(MODEL_MCP_URL, data=body,
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            doc = json.loads(resp.read().decode())
    except Exception as exc:
        return "", "model_unavailable: " + str(exc)[:200]
    result = (doc.get("result") or {})
    if result.get("isError"):
        text = (result.get("content") or [{}])[0].get("text", "")
        return "", "model_error: " + text[:200]
    try:
        inner = json.loads((result.get("content") or [{}])[0].get("text") or "{}")
    except json.JSONDecodeError:
        return "", "model_error: unreadable completion"
    content = (inner.get("content") or "").strip()
    if inner.get("mock"):
        return "", "model_returned_no_sql: the model service is unconfigured (MODEL_BASE_URL)"
    if not content:
        return "", "model_returned_no_sql"
    if content.strip().rstrip(";").upper() == "NONE":
        return "", "no_table_can_answer_this: the model found no table for the question"
    # Strip fences a model likes to add, then take the first statement only.
    cleaned = content.replace("```sql", "```").split("```")
    sql = max(cleaned, key=len).strip() if "```" in content else content
    return sql.split(";")[0].strip() + (";" if sql.rstrip().endswith(";") else ""), ""


def ask(args: dict) -> dict:
    question = str(args.get("question") or "").strip()
    if not question:
        return {"is_error": True, "error": "question is required"}
    sql, why = _model_sql(question)
    if why:
        return {"is_error": True, "error": why}
    out = run_sql(sql, args.get("limit") or DEFAULT_LIMIT)
    out["question"] = question
    return out


def handle(body: dict) -> dict:
    method, rid = body.get("method"), body.get("id")

    def ok(result):
        return {"jsonrpc": "2.0", "id": rid, "result": result}

    if method == "initialize":
        return ok({"protocolVersion": "2024-11-05",
                   "serverInfo": {"name": "clickhouse", "version": "1.0.0"},
                   "capabilities": {"tools": {}}})
    if method in ("notifications/initialized", "ping"):
        return ok({})
    if method == "tools/list":
        return ok({"tools": TOOLS})
    if method == "tools/call":
        params = body.get("params") or {}
        name, args = params.get("name"), (params.get("arguments") or {})
        try:
            if name == "ch_query":
                out = run_sql(args.get("sql", ""), args.get("limit") or DEFAULT_LIMIT)
            elif name == "ch_schema":
                out = {"is_error": False, "schema": schema_text(args.get("table", ""))}
            elif name == "nlp_ask":
                out = ask(args)
            else:
                return ok({"isError": True, "content": [{"type": "text", "text": "unknown tool"}]})
        except Exception as exc:  # never swallow: the caller sees the reason
            return ok({"isError": True, "content": [{"type": "text", "text": str(exc)[:300]}]})
        if out.pop("is_error"):
            return ok({"isError": True,
                       "content": [{"type": "text", "text": json.dumps(out, ensure_ascii=False)}]})
        return ok({"isError": False,
                   "content": [{"type": "text", "text": json.dumps(out, ensure_ascii=False)}]})
    return {"jsonrpc": "2.0", "id": rid, "error": {"code": -32601, "message": "method not found"}}


class Handler(BaseHTTPRequestHandler):
    def do_POST(self) -> None:
        if self.path.rstrip("/") != "/mcp":
            return self._json(404, {"error": "not_found"})
        size = int(self.headers.get("Content-Length", "0"))
        try:
            body = json.loads(self.rfile.read(size) or b"{}")
        except json.JSONDecodeError:
            return self._json(400, {"error": "bad_json"})
        self._json(200, handle(body))

    def do_GET(self) -> None:
        if self.path.rstrip("/") == "/health":
            try:
                _ch("SELECT 1", {"max_execution_time": "2"}, 3)
                return self._json(200, {"ok": True, "clickhouse": "up", "database": CH_DATABASE})
            except Exception as exc:
                # The service is up; the database is not. Saying "ok" here would make the gateway
                # advertise a tool that cannot answer.
                return self._json(503, {"ok": False, "clickhouse": "down",
                                        "error": _ch_error(exc)[:200]})
        return self._json(404, {"error": "not_found"})

    def _json(self, code: int, body: dict) -> None:
        raw = json.dumps(body, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *args) -> None:
        return


def main() -> None:
    port = int(os.environ.get("PORT", "8098"))
    print(f"clickhouse-mcp on :{port} → {CH_URL} (db {CH_DATABASE})", flush=True)
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
