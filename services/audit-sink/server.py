"""Audit sink: one place where several gateways write, so "how many requests were shed today"
has an answer that is not "grep three files".

The gateway always writes its own JSONL file first: that file is the durable record, because an
audit trail that depends on a network hop is a trail with a hole in it. This service is the
aggregate view — it appends what it receives, drops duplicates by content hash (a retry is not a
second event), and answers counts.

Endpoints:
  POST /v1/audit    one JSON object, or {"lines": [...]}  → {accepted, duplicates}
  GET  /v1/report   counts by decision / tool / user for a day
  GET  /health      line count, duplicate count, store
"""
from __future__ import annotations

import hashlib
import json
import os
import sqlite3
import threading
import time
from collections import Counter
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parent
DB_PATH = Path(os.environ.get("AUDIT_DB", ROOT / "state" / "audit.db"))
PORT = int(os.environ.get("PORT", "8096"))
HOST = os.environ.get("AUDIT_HOST") or os.environ.get("HOST") or "127.0.0.1"
MAX_BYTES = 512 * 1024

SCHEMA = """
CREATE TABLE IF NOT EXISTS events (
    hash        TEXT PRIMARY KEY,
    day         TEXT NOT NULL,
    ts          TEXT NOT NULL,
    user_id     TEXT NOT NULL DEFAULT '',
    chat        TEXT NOT NULL DEFAULT '',
    decision    TEXT NOT NULL DEFAULT '',
    tool        TEXT NOT NULL DEFAULT '',
    run_id      TEXT NOT NULL DEFAULT '',
    body        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_day ON events(day);
"""


class Store:
    def __init__(self, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        self.db = sqlite3.connect(str(path), check_same_thread=False)
        self.db.row_factory = sqlite3.Row
        self.lock = threading.RLock()
        self.db.executescript(SCHEMA)
        # A database written by an older build has no run_id column. Adding it is cheap; a silent
        # "no such column" on the first insert is not.
        have = {row["name"] for row in self.db.execute("PRAGMA table_info(events)")}
        if "run_id" not in have:
            self.db.execute("ALTER TABLE events ADD COLUMN run_id TEXT NOT NULL DEFAULT ''")
        self.db.execute("CREATE INDEX IF NOT EXISTS events_run ON events(run_id)")
        self.db.commit()

    @staticmethod
    def canonical(event: dict) -> tuple[str, str]:
        """Identity of an event.

        An `event_id` from the gateway is the identity: a retried delivery carries the same id,
        and two requests in the same second carry different ones. Without an id (an older line, a
        hand-written fixture) fall back to the content *including* the timestamp — dropping the
        timestamp there was a bug the tests caught: four distinct events collapsed into two
        because they only differed by time.
        """
        raw = json.dumps(event, sort_keys=True, ensure_ascii=False)
        identity = event.get("event_id") or raw
        return hashlib.sha256(str(identity).encode()).hexdigest(), raw

    def add(self, events: list[dict]) -> tuple[int, int]:
        accepted = duplicates = 0
        now = datetime.now(timezone.utc)
        with self.lock:
            for event in events:
                digest, raw = self.canonical(event)
                # The day is part of the hash: the same request legitimately happens again
                # tomorrow, and dedupe must not swallow it.
                day = str(event.get("ts", ""))[:10] or now.strftime("%Y-%m-%d")
                digest = hashlib.sha256((day + digest).encode()).hexdigest()
                try:
                    self.db.execute(
                        "INSERT INTO events(hash, day, ts, user_id, chat, decision, tool, run_id, body) "
                        "VALUES(?,?,?,?,?,?,?,?,?)",
                        (digest, day, str(event.get("ts", now.isoformat())),
                         str(event.get("user", "")), str(event.get("chat", "")),
                         str(event.get("decision", "")), str(event.get("tool", "")),
                         str(event.get("run_id", "")), raw))
                    accepted += 1
                except sqlite3.IntegrityError:
                    duplicates += 1
            self.db.commit()
        return accepted, duplicates

    def report(self, day: str) -> dict:
        with self.lock:
            rows = self.db.execute(
                "SELECT decision, tool, user_id, run_id FROM events WHERE day=?",
                (day,)).fetchall()
        decisions, tools, users = Counter(), Counter(), Counter()
        for r in rows:
            decisions[r["decision"]] += 1
            if r["tool"]:
                tools[r["tool"]] += 1
            users[r["user_id"]] += 1
        total = len(rows)
        # Runs, not events: the day view should say how many pieces of work happened, because
        # that is the number a person compares to yesterday.
        runs = len({r["run_id"] for r in rows if r["run_id"]})
        shed = decisions.get("shed", 0)
        denied = decisions.get("denied", 0) + decisions.get("tool_not_allowed", 0)
        return {
            "day": day, "events": total, "runs": runs,
            "decisions": dict(decisions.most_common()),
            "tools": dict(tools.most_common()),
            "users": dict(users.most_common()),
            # Rates, not raw counts: "3 shed" means nothing without the denominator.
            "shed_rate": round(shed / total, 4) if total else 0.0,
            "denied_rate": round(denied / total, 4) if total else 0.0,
        }

    def run(self, run_id: str) -> dict:
        """Every step of one piece of work, in the order it happened.

        This is the query the run id exists for. Without it the record answers "what happened
        today"; with it the record answers "what happened to *this* request", which is what a
        support ticket asks and what no sampled trace can answer a week later.
        """
        if not run_id:
            return {"run_id": "", "events": [], "error": "run_id is required"}
        with self.lock:
            rows = self.db.execute(
                "SELECT ts, user_id, chat, decision, tool, body FROM events "
                "WHERE run_id=? ORDER BY ts, rowid", (run_id,)).fetchall()
        events = []
        for r in rows:
            event = {"ts": r["ts"], "user": r["user_id"], "chat": r["chat"],
                     "decision": r["decision"], "tool": r["tool"]}
            try:
                body = json.loads(r["body"])
                event["event"] = body.get("event", "")
                event["event_id"] = body.get("event_id", "")
                for k in ("role", "reason", "cache", "job", "state", "code", "ms"):
                    if k in body:
                        event[k] = body[k]
            except (ValueError, TypeError):
                pass
            events.append(event)
        return {"run_id": run_id, "count": len(events), "events": events}

    def health(self) -> dict:
        with self.lock:
            rows = self.db.execute("SELECT COUNT(*) AS n FROM events").fetchone()["n"]
        return {"ok": True, "service": "audit-sink", "events": rows, "db": str(DB_PATH)}


STORE = Store(DB_PATH)


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
        path = self.path.split("?")[0].rstrip("/") or "/"
        if path == "/health":
            return self._json(200, STORE.health())
        if path == "/v1/report":
            query = dict(part.split("=", 1) for part in self.path.split("?")[1].split("&")
                         if "=" in part) if "?" in self.path else {}
            day = query.get("day") or datetime.now(timezone.utc).strftime("%Y-%m-%d")
            return self._json(200, STORE.report(day))
        if path == "/v1/run":
            query = dict(part.split("=", 1) for part in self.path.split("?")[1].split("&")
                         if "=" in part) if "?" in self.path else {}
            return self._json(200, STORE.run(query.get("run_id", "")))
        self._json(404, {"error": "not_found"})

    def do_POST(self) -> None:
        if self.path.rstrip("/") != "/v1/audit":
            return self._json(404, {"error": "not_found"})
        n = int(self.headers.get("Content-Length", "0"))
        if n > MAX_BYTES:
            return self._json(413, {"error": "payload_too_large"})
        try:
            payload = json.loads(self.rfile.read(n) or b"{}")
        except json.JSONDecodeError:
            return self._json(400, {"error": "bad_json"})
        events = payload.get("lines") if isinstance(payload, dict) and "lines" in payload else [payload]
        if not isinstance(events, list) or not all(isinstance(e, dict) for e in events):
            return self._json(400, {"error": "expected an object or a list of objects"})
        accepted, duplicates = STORE.add(events)
        self._json(202, {"accepted": accepted, "duplicates": duplicates})

    def log_message(self, fmt: str, *args) -> None:
        return


if __name__ == "__main__":
    print(f"audit-sink on {HOST}:{PORT} — {STORE.health()}", flush=True)
    ThreadingHTTPServer((HOST, PORT), Handler).serve_forever()
