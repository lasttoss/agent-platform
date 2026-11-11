"""Three-layer cache for the gateway.

Why three layers, and what each one buys:

  L1 exact    same normalised request, same tool set, same policy version  -> no model call at all
  L2 semantic near-duplicate question in the same scope                    -> no model call, answer reused
  L3 prefix   the stable part of the prompt is byte-identical every time   -> provider-side prefix cache
                                                                             pays for the static part once

L1 and L2 are implemented here with SQLite. L3 is a property of how the prompt is
assembled (see `prefix_fingerprint`): if the tool list order or the policy version
changes the prefix, the provider cache misses, so both are put in a fixed order.

Scope is the security boundary of the cache, not an afterthought: a personal answer is
stored under `user:<id>` and is never returned to a different user.
"""
from __future__ import annotations

import hashlib
import json
import math
import re
import sqlite3
import time
import unicodedata
from pathlib import Path

DIM = 256
SEMANTIC_THRESHOLD = 0.93


def fold(text: str) -> str:
    """Strip Vietnamese diacritics so "lương", "luong" and "LUONG" are the same request.

    People type without tones on a phone keyboard; a gateway that only matches the
    accented spelling answers "no tool for this" to a question it fully understands.
    """
    decomposed = unicodedata.normalize("NFD", text)
    stripped = "".join(c for c in decomposed if not unicodedata.combining(c))
    return stripped.replace("\u0111", "d").replace("\u0110", "D").lower()


def normalise(text: str) -> str:
    return re.sub(r"\s+", " ", fold(text.strip()))


def _hash_key(payload: dict) -> str:
    return hashlib.sha256(json.dumps(payload, sort_keys=True, ensure_ascii=False).encode()).hexdigest()


def exact_key(text: str, tools: list[str], policy_version: str, chat_type: str, scope: str) -> str:
    return _hash_key({
        "text": normalise(text),
        "tools": sorted(tools),          # order must not change the key
        "policy": policy_version,
        "chat": chat_type,
        "scope": scope,
    })


def prefix_fingerprint(tools: list[str], policy_version: str) -> str:
    """L3: the part of the prompt that must stay byte-identical to keep the provider
    prefix cache warm. Sorted, versioned, no timestamps, no user data."""
    return _hash_key({"tools": sorted(tools), "policy": policy_version})


def embed(text: str) -> list[float]:
    """Deterministic hashed bag-of-words embedding. No dependencies, no network.

    This is a similarity proxy, not a semantic model: it catches rewordings that keep
    most of the same words ("lương tháng này" vs "tháng này lương"), not paraphrases
    with no shared vocabulary. Swapping in a real embedding service means implementing
    `embed` against it — the store and the threshold logic do not change.
    """
    vec = [0.0] * DIM
    for token in re.findall(r"\w+", normalise(text)):
        vec[hash(token) % DIM] += 1.0
    norm = math.sqrt(sum(v * v for v in vec)) or 1.0
    return [v / norm for v in vec]


def cosine(a: list[float], b: list[float]) -> float:
    return sum(x * y for x, y in zip(a, b))


class CacheStore:
    def __init__(self, path: str | Path) -> None:
        self.path = str(path)
        self.db = sqlite3.connect(self.path, check_same_thread=False)
        self.db.executescript(
            """
            CREATE TABLE IF NOT EXISTS exact(
                k TEXT PRIMARY KEY, scope TEXT, answer TEXT, tools TEXT,
                tokens_saved INTEGER, hits INTEGER DEFAULT 0, created_at REAL, ttl_s INTEGER
            );
            CREATE TABLE IF NOT EXISTS semantic(
                id INTEGER PRIMARY KEY AUTOINCREMENT, scope TEXT, text TEXT,
                vec TEXT, answer TEXT, tokens_saved INTEGER, created_at REAL, ttl_s INTEGER
            );
            CREATE TABLE IF NOT EXISTS stats(
                k TEXT PRIMARY KEY, v INTEGER
            );
            """
        )
        self.db.commit()

    # ---------- L1 ----------
    def get_exact(self, key: str) -> dict | None:
        row = self.db.execute(
            "SELECT answer, tokens_saved, created_at, ttl_s, scope FROM exact WHERE k=?", (key,)
        ).fetchone()
        if not row:
            self.bump("l1_miss")
            return None
        answer, saved, created, ttl_s, scope = row
        if ttl_s >= 0 and time.time() - created > ttl_s:
            self.db.execute("DELETE FROM exact WHERE k=?", (key,))
            self.db.commit()
            self.bump("l1_expired")
            return None
        self.db.execute("UPDATE exact SET hits = hits + 1 WHERE k=?", (key,))
        self.db.commit()
        self.bump("l1_hit")
        return {"layer": "exact", "answer": answer, "saved_tokens": saved, "scope": scope}

    def put_exact(self, key: str, answer: str, tools: list[str], tokens: int, ttl_s: int, scope: str) -> None:
        if ttl_s == 0:            # category opted out of caching
            return
        self.db.execute(
            "REPLACE INTO exact(k, scope, answer, tools, tokens_saved, created_at, ttl_s) VALUES(?,?,?,?,?,?,?)",
            (key, scope, answer, json.dumps(sorted(tools)), tokens, time.time(), ttl_s),
        )
        self.db.commit()

    # ---------- L2 ----------
    def get_semantic(self, text: str, scope: str, threshold: float = SEMANTIC_THRESHOLD) -> dict | None:
        vec = embed(text)
        best, best_sim = None, 0.0
        now = time.time()
        for row_id, old_text, old_vec, answer, saved, created, ttl_s in self.db.execute(
            "SELECT id, text, vec, answer, tokens_saved, created_at, ttl_s FROM semantic WHERE scope=?", (scope,)
        ):
            if ttl_s >= 0 and now - created > ttl_s:
                self.db.execute("DELETE FROM semantic WHERE id=?", (row_id,))
                continue
            sim = cosine(vec, json.loads(old_vec))
            if sim > best_sim:
                best, best_sim = (answer, saved, old_text), sim
        self.db.commit()
        if best and best_sim >= threshold:
            self.bump("l2_hit")
            return {"layer": "semantic", "answer": best[0], "saved_tokens": best[1],
                    "matched": best[2], "similarity": round(best_sim, 3), "scope": scope}
        self.bump("l2_miss")
        return None

    def put_semantic(self, text: str, answer: str, tokens: int, ttl_s: int, scope: str) -> None:
        if ttl_s == 0:
            return
        self.db.execute(
            "INSERT INTO semantic(scope, text, vec, answer, tokens_saved, created_at, ttl_s) VALUES(?,?,?,?,?,?,?)",
            (scope, normalise(text), json.dumps(embed(text)), answer, tokens, time.time(), ttl_s),
        )
        self.db.commit()

    # ---------- stats ----------
    def bump(self, name: str, by: int = 1) -> None:
        self.db.execute("INSERT INTO stats(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = v + ?",
                        (name, by, by))
        self.db.commit()

    def stats(self) -> dict:
        rows = dict(self.db.execute("SELECT k, v FROM stats").fetchall())
        l1 = rows.get("l1_hit", 0)
        l1_total = l1 + rows.get("l1_miss", 0)
        l2 = rows.get("l2_hit", 0)
        l2_total = l2 + rows.get("l2_miss", 0)
        saved = self.db.execute("SELECT COALESCE(SUM(tokens_saved),0) FROM exact").fetchone()[0]
        saved += self.db.execute("SELECT COALESCE(SUM(tokens_saved),0) FROM semantic").fetchone()[0]
        return {
            "l1_hit": l1, "l1_hit_rate": round(l1 / l1_total, 3) if l1_total else 0.0,
            "l2_hit": l2, "l2_hit_rate": round(l2 / l2_total, 3) if l2_total else 0.0,
            "tokens_saved_estimate": saved,
        }
