"""User directory service: Postgres decides who exists, and whether they may act.

Every service in this repo keeps its own records — the OCR service has jobs, the demo has salary
rows, the gateway has consent grants. None of them is the place to answer "is this person still
allowed to use the platform": that answer has to come from one store, and disabling a person there
has to take effect everywhere.

Two things follow from that, and they are the whole point of this service:

1. the gateway asks this service, on a timer, for the list of enabled users;
2. the same service can compare itself against the other stores and name the rows that disagree.

Storage is Postgres when `USERDIR_DSN` is set, SQLite otherwise. The fallback exists for tests
and for the demo, and it is *reported* over the wire (`store` in every answer) so a deployment can
never mistake one for the other. There is no silent fallback: a configured DSN that cannot be
opened stops the service instead of quietly serving a local copy.

MCP tools:
  user_list          the directory, as the gateway sees it
  user_consistency   compare the directory against a list of ids another system believes in
  user_set_active    enable/disable a person (destructive: never reachable from a chat)
"""
from __future__ import annotations

import json
import os
import sqlite3
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parent
DSN = os.environ.get("USERDIR_DSN", "").strip()
DB_PATH = Path(os.environ.get("USERDIR_DB", ROOT / "state" / "users.db"))
PORT = int(os.environ.get("PORT", "8093"))
MAX_BYTES = 64 * 1024

SCHEMA = """
CREATE TABLE IF NOT EXISTS users (
    id          TEXT PRIMARY KEY,
    active      INTEGER NOT NULL DEFAULT 1,
    role        TEXT NOT NULL DEFAULT 'dev',
    workspace   TEXT NOT NULL DEFAULT '',
    updated_at  REAL NOT NULL DEFAULT 0
)
"""


class SQLiteStore:
    kind = "sqlite"

    def __init__(self, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        self.db = sqlite3.connect(str(path), check_same_thread=False)
        self.db.row_factory = sqlite3.Row
        self.lock = threading.RLock()
        self.db.execute(SCHEMA)
        self.db.commit()

    def all_users(self) -> list[dict]:
        with self.lock:
            rows = self.db.execute(
                "SELECT id, active, role, workspace, updated_at FROM users ORDER BY id").fetchall()
        return [dict(r) for r in rows]

    def set_active(self, user_id: str, active: bool) -> dict:
        with self.lock:
            self.db.execute(
                "INSERT INTO users(id, active, role, workspace, updated_at) VALUES(?,?,?,?,?) "
                "ON CONFLICT(id) DO UPDATE SET active=excluded.active, updated_at=excluded.updated_at",
                (user_id, 1 if active else 0, "dev", "", time.time()))
            self.db.commit()
        return {"id": user_id, "active": active, "store": self.kind}


class PostgresStore:
    kind = "postgres"

    def __init__(self, dsn: str) -> None:
        try:
            import psycopg  # type: ignore
        except ImportError as exc:  # pragma: no cover - depends on the deployment image
            raise SystemExit(
                "USERDIR_DSN is set but psycopg is not installed: pip install -r requirements-pg.txt. "
                "Refusing to fall back to a local file, because a disabled user in Postgres must "
                "disable that user here too.") from exc
        self.psycopg = psycopg
        self.conn = psycopg.connect(dsn, autocommit=True)
        with self.conn.cursor() as cur:
            cur.execute(SCHEMA.replace("INTEGER", "BOOLEAN").replace("REAL", "DOUBLE PRECISION"))

    def all_users(self) -> list[dict]:
        with self.conn.cursor() as cur:
            cur.execute("SELECT id, active, role, workspace, updated_at FROM users ORDER BY id")
            return [{"id": r[0], "active": bool(r[1]), "role": r[2], "workspace": r[3],
                     "updated_at": r[4]} for r in cur.fetchall()]

    def set_active(self, user_id: str, active: bool) -> dict:
        with self.conn.cursor() as cur:
            cur.execute(
                "INSERT INTO users(id, active, role, workspace, updated_at) VALUES(%s,%s,%s,%s,%s) "
                "ON CONFLICT (id) DO UPDATE SET active=EXCLUDED.active, updated_at=EXCLUDED.updated_at",
                (user_id, active, "dev", "", time.time()))
        return {"id": user_id, "active": active, "store": self.kind}


STORE = PostgresStore(DSN) if DSN else SQLiteStore(DB_PATH)


def tool_user_list(_args: dict) -> dict:
    users = STORE.all_users()
    return {"store": STORE.kind, "count": len(users),
            "users": [{"id": u["id"], "active": bool(u["active"]),
                       "role": u["role"], "workspace": u["workspace"]} for u in users]}


def tool_user_consistency(args: dict) -> dict:
    """Compare the directory against ids another system believes in.

    The comparison is deliberately symmetrical: a person the other system knows but the directory
    does not is as broken as a person the directory keeps active and nobody can find. One of those
    two directions is usually a forgotten offboarding.
    """
    known = [str(x) for x in (args.get("known_ids") or [])]
    users = {u["id"]: u for u in STORE.all_users()}
    missing_in_directory = sorted(i for i in known if i not in users)
    unknown_in_caller = sorted(i for i in users if i not in known)
    inactive = sorted(i for i, u in users.items() if not u["active"])
    return {
        "store": STORE.kind,
        "checked": len(known),
        "missing_in_directory": missing_in_directory,
        "unknown_in_caller": unknown_in_caller,
        "inactive": inactive,
        "match": not missing_in_directory and not unknown_in_caller,
    }


def tool_user_set_active(args: dict) -> dict:
    user_id = str(args.get("user_id", "")).strip()
    if not user_id:
        raise ValueError("user_id is required")
    if not user_id.isprintable() or len(user_id) > 64:
        raise ValueError("user_id must be printable and at most 64 characters")
    return STORE.set_active(user_id, bool(args.get("active", True)))


TOOLS = [
    {"name": "user_list", "description": "List the user directory.",
     "inputSchema": {"type": "object", "properties": {}}},
    {"name": "user_consistency", "description": "Compare the directory with another system's ids.",
     "inputSchema": {"type": "object",
                     "properties": {"known_ids": {"type": "array", "items": {"type": "string"}}},
                     "required": ["known_ids"]}},
    {"name": "user_set_active", "description": "Enable or disable a user (destructive).",
     "inputSchema": {"type": "object",
                     "properties": {"user_id": {"type": "string"}, "active": {"type": "boolean"}},
                     "required": ["user_id", "active"]}},
]

HANDLERS = {"user_list": tool_user_list, "user_consistency": tool_user_consistency,
            "user_set_active": tool_user_set_active}


def handle(body: dict) -> dict:
    method, rid = body.get("method"), body.get("id")

    def ok(result: object) -> dict:
        return {"jsonrpc": "2.0", "id": rid, "result": result}

    if method == "initialize":
        return ok({"protocolVersion": "2024-11-05",
                   "serverInfo": {"name": "userdir", "version": "1.0.0"},
                   "capabilities": {"tools": {}}})
    if method in ("notifications/initialized", "ping"):
        return ok({})
    if method == "tools/list":
        return ok({"tools": TOOLS})
    if method == "tools/call":
        params = body.get("params") or {}
        name = params.get("name")
        fn = HANDLERS.get(str(name))
        if fn is None:
            payload = {"isError": True, "error": f"unknown tool: {name}"}
        else:
            try:
                payload = {"isError": False, **fn(params.get("arguments") or {})}
            except ValueError as exc:
                payload = {"isError": True, "error": str(exc)}
        return ok({"isError": bool(payload.get("isError")),
                   "content": [{"type": "text", "text": json.dumps(payload, ensure_ascii=False)}]})
    return {"jsonrpc": "2.0", "id": rid,
            "error": {"code": -32601, "message": f"method not found: {method}"}}


class Handler(BaseHTTPRequestHandler):
    request_queue_size = 64

    def _json(self, code: int, body: dict) -> None:
        raw = json.dumps(body, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self) -> None:
        if self.path.rstrip("/") in ("/health", ""):
            return self._json(200, {"ok": True, "service": "userdir", "store": STORE.kind,
                                    "users": len(STORE.all_users())})
        self._json(404, {"error": "not_found"})

    def do_POST(self) -> None:
        if self.path.rstrip("/") != "/mcp":
            return self._json(404, {"error": "not_found"})
        n = int(self.headers.get("Content-Length", "0"))
        if n > MAX_BYTES:
            return self._json(413, {"error": "payload_too_large"})
        try:
            payload = json.loads(self.rfile.read(n) or b"{}")
        except json.JSONDecodeError:
            return self._json(400, {"error": "bad_json"})
        self._json(200, handle(payload))

    def log_message(self, fmt: str, *args) -> None:
        return


if __name__ == "__main__":
    print(f"userdir on :{PORT} — store={STORE.kind} users={len(STORE.all_users())}")
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
