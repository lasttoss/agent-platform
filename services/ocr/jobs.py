"""Job queue for document extraction.

Why a queue and not just a slower request: a scan takes 2-5 seconds per file, so ten files in
one request is a 30-50 second HTTP call that will time out somewhere in the middle and lose the
work. The queue makes progress durable and lets the caller poll.

What is also here on purpose: an expiry. Extracted text is somebody else's personal document —
it is kept for a bounded time and then removed, while the job row stays so the caller learns
"expired" instead of "not found".
"""
from __future__ import annotations

import json
import queue
import sqlite3
import threading
import time
import uuid
from pathlib import Path

QUEUE_MAX = 50
TTL_S = 3600


class JobStore:
    def __init__(self, db_path: str | Path, ttl_s: int = TTL_S, workers: int = 1,
                 maxsize: int = QUEUE_MAX) -> None:
        Path(db_path).parent.mkdir(parents=True, exist_ok=True)
        self.db = sqlite3.connect(str(db_path), check_same_thread=False)
        self.db.executescript(
            """
            CREATE TABLE IF NOT EXISTS jobs(
                id TEXT PRIMARY KEY, path TEXT, lang TEXT, status TEXT,
                created_at REAL, finished_at REAL, result TEXT, error TEXT, expires_at REAL
            );
            """
        )
        self.db.commit()
        self.ttl_s = ttl_s
        self.q: queue.Queue[str] = queue.Queue(maxsize=maxsize)
        self.workers = [threading.Thread(target=self._work, daemon=True) for _ in range(workers)]
        for w in self.workers:
            w.start()

    # ---------- producer ----------
    def submit(self, path: str, lang: str) -> dict:
        job_id = uuid.uuid4().hex[:12]
        self.db.execute(
            "INSERT INTO jobs(id, path, lang, status, created_at) VALUES(?,?,?,?,?)",
            (job_id, path, lang, "queued", time.time()),
        )
        self.db.commit()
        try:
            self.q.put_nowait(job_id)          # a full queue is a 503, never a silent drop
        except queue.Full:
            self.db.execute("UPDATE jobs SET status='rejected', error='queue_full' WHERE id=?", (job_id,))
            self.db.commit()
            return {"id": job_id, "status": "rejected", "error": "queue_full"}
        return {"id": job_id, "status": "queued", "poll": f"/v1/jobs/{job_id}"}

    # ---------- consumer ----------
    def _work(self) -> None:
        from server import extract_for_job          # local import: server owns the guards
        while True:
            job_id = self.q.get()
            row = self.db.execute("SELECT path, lang FROM jobs WHERE id=?", (job_id,)).fetchone()
            if row is None:
                continue
            path, lang = row
            self.db.execute("UPDATE jobs SET status='running' WHERE id=?", (job_id,))
            self.db.commit()
            try:
                result = extract_for_job(path, lang)
                status, payload = "done", result
            except Exception as exc:                 # keep the reason, do not swallow it
                status, payload = "failed", {"error": type(exc).__name__, "detail": str(exc)[:300]}
            self.db.execute(
                "UPDATE jobs SET status=?, finished_at=?, result=?, expires_at=? WHERE id=?",
                (status, time.time(), json.dumps(payload, ensure_ascii=False),
                 time.time() + self.ttl_s, job_id),
            )
            self.db.commit()

    # ---------- reader ----------
    def get(self, job_id: str) -> dict | None:
        row = self.db.execute(
            "SELECT id, path, status, created_at, finished_at, result, error, expires_at "
            "FROM jobs WHERE id=?", (job_id,)).fetchone()
        if row is None:
            return None
        jid, path, status, created, finished, result, error, expires = row
        if status in ("done", "failed") and expires and time.time() > expires:
            # TTL: the document text is gone, the job is not. The caller can re-submit.
            self.db.execute("UPDATE jobs SET status='expired', result=NULL WHERE id=?", (jid,))
            self.db.commit()
            return {"id": jid, "status": "expired", "file": path,
                    "note": "extracted text was removed after its retention window"}
        out = {"id": jid, "status": status, "file": path, "created_at": created,
               "finished_at": finished, "expires_at": expires}
        if error:
            out["error"] = error
        if result:
            out["result"] = json.loads(result)
        return out

    def purge(self) -> int:
        """Remove expired text even if nobody reads the job again."""
        cur = self.db.execute(
            "UPDATE jobs SET status='expired', result=NULL "
            "WHERE status IN ('done','failed') AND expires_at IS NOT NULL AND expires_at < ?",
            (time.time(),))
        self.db.commit()
        return cur.rowcount
