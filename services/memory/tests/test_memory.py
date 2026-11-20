"""These tests are about the four ways a memory store goes wrong quietly.

Not "does pgvector work" -- it does. The failures worth a test are the ones that look like success:
someone reading somebody else's memory, a fact that duplicates instead of replacing, expired rows
that keep coming back because the cleaner is late, and a table that grows because a tool result
was inlined instead of pointed at.

They need a Postgres with pgvector (`MEMORY_TEST_DSN`). Without it they skip, and the skip is
visible in the output -- a green run on a machine with no database must not read as "the isolation
test passed".
"""
from __future__ import annotations

import os
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))

import memory as mem            # noqa: E402
import toolref                  # noqa: E402

DSN = os.environ.get("MEMORY_TEST_DSN", "").strip()
DIM = 8                          # small enough for hand-written vectors, same code path


def make_store() -> "mem.Memory":
    store = mem.Memory(DSN, dim=DIM)
    with store._conn() as conn:
        # Both tables: an audit row from a previous test would otherwise make a later test pass
        # or fail for a reason that has nothing to do with what it is checking.
        conn.execute("DROP TABLE IF EXISTS memory CASCADE")
        conn.execute("DROP TABLE IF EXISTS memory_event CASCADE")
        conn.execute("DROP TABLE IF EXISTS memory_archive CASCADE")
    store.create_schema()
    return store


@unittest.skipUnless(DSN, "MEMORY_TEST_DSN not set: set it to run the isolation tests")
class MemoryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.store = make_store()
        self.alice = self.store.scope("alice")
        self.bob = self.store.scope("bob")

    # -- the one that matters ---------------------------------------------------------------
    def test_a_user_cannot_read_another_users_memory(self) -> None:
        """Isolation is a WHERE clause. If it is ever dropped, this test is the only thing that says so."""
        self.alice.put("fact", "dau-11", "vụ dâu tháng 11 giảm 12%", embedding=[1, 0, 0, 0, 0, 0, 0, 0])

        self.assertIsNone(self.bob.get("fact", "dau-11"))
        self.assertEqual(self.bob.search([1, 0, 0, 0, 0, 0, 0, 0]), [])
        self.assertEqual(self.bob.count(), 0)
        # ...while the owner still sees it, so the test cannot pass by storing nothing.
        self.assertEqual(self.alice.get("fact", "dau-11"), "vụ dâu tháng 11 giảm 12%")

    def test_a_scope_without_an_identity_is_refused(self) -> None:
        """Identity is built from an authenticated value; an empty one is a bug, not a default."""
        for bad in ("", "   ", None):
            with self.assertRaises(mem.MemoryError):
                self.store.scope(bad)   # type: ignore[arg-type]

    def test_forget_only_touches_the_callers_rows(self) -> None:
        self.alice.put("fact", "a", "alice")
        self.bob.put("fact", "a", "bob")

        self.assertEqual(self.alice.forget(), 1)
        self.assertEqual(self.alice.count(), 0)
        self.assertEqual(self.bob.get("fact", "a"), "bob")

    # -- growth ----------------------------------------------------------------------------
    def test_putting_the_same_key_twice_replaces_the_older_fact(self) -> None:
        """Memory that only appends is a log. The second write must replace, and say so."""
        self.assertTrue(self.alice.put("fact", "dau-11", "giảm 12%"))
        self.assertFalse(self.alice.put("fact", "dau-11", "giảm 15% sau kiểm kê"))

        self.assertEqual(self.alice.count("fact"), 1)
        self.assertEqual(self.alice.get("fact", "dau-11"), "giảm 15% sau kiểm kê")

    def test_a_body_over_the_cap_is_refused(self) -> None:
        """A 200 KB write fails loudly here instead of becoming a 200 KB row nobody notices."""
        with self.assertRaises(mem.MemoryError):
            self.alice.put("tool_result", "dump", "x" * (mem.BODY_LIMIT + 1))

    def test_a_large_tool_result_is_stored_as_a_pointer(self) -> None:
        """The row's size must not follow the result's size."""
        spool = Path(tempfile.mkdtemp())
        payload = "dòng,dữ_liệu\n" * 20_000            # ~260 KB
        ref = toolref.spool(payload, "export.csv", spool)

        self.alice.put("tool_result", "export", toolref.dumps(ref))
        self.assertEqual(self.alice.count("tool_result"), 1)

        with self.store._conn() as conn:
            row = conn.execute(
                "SELECT octet_length(body) AS n FROM memory WHERE user_id='alice'"
            ).fetchone()
        self.assertLess(row["n"], 512)
        self.assertEqual(ref["bytes"], len(payload.encode()))
        self.assertTrue(toolref.verify(ref))
        # 7 bytes is exactly "dòng,d"; 8 and 9 land inside the 3-byte "ữ" and must not
        # produce a replacement character at the end of the read.
        for ceiling in (7, 8, 9):
            with self.subTest(ceiling=ceiling):
                self.assertEqual(toolref.read(toolref.dumps(ref), max_bytes=ceiling), "dòng,d")

    def test_the_same_tool_output_spools_to_one_file(self) -> None:
        """Content addressed: writing it twice must not cost twice."""
        spool = Path(tempfile.mkdtemp())
        a = toolref.spool("same bytes", "out.txt", spool)
        b = toolref.spool("same bytes", "out.txt", spool)
        self.assertEqual(a["path"], b["path"])
        self.assertEqual(len(os.listdir(spool)), 1)

    # -- expiry ----------------------------------------------------------------------------
    def test_an_expired_fact_disappears_from_reads_before_the_sweeper_runs(self) -> None:
        """The cleaner is a job and jobs are late. Expiry has to hold on the read path too."""
        self.alice.put("fact", "old", "hết hạn", ttl_seconds=-1)
        self.alice.put("fact", "live", "còn hạn", ttl_seconds=3600)

        self.assertIsNone(self.alice.get("fact", "old"))
        self.assertEqual(self.alice.count(), 2)          # still on disk, invisible to reads
        self.assertEqual(self.store.sweep(), 1)
        self.assertEqual(self.alice.count(), 1)

    # -- retrieval -------------------------------------------------------------------------
    def test_search_returns_the_nearest_rows_first(self) -> None:
        near = [1, 0, 0, 0, 0, 0, 0, 0]
        far = [0, 1, 0, 0, 0, 0, 0, 0]
        self.alice.put("fact", "near", "gần", embedding=near)
        self.alice.put("fact", "far", "xa", embedding=far)

        hits = self.alice.search([1, 0, 0, 0, 0, 0, 0, 0], k=2)
        self.assertEqual([h.key for h in hits], ["near", "far"])
        self.assertGreater(hits[0].score, hits[1].score)

    def test_search_respects_kind_and_leaves_unembedded_rows_out(self) -> None:
        """A preference found by key is not a similarity search; it must not pollute one."""
        vec = [1, 0, 0, 0, 0, 0, 0, 0]
        self.alice.put("pref", "theme", "dark")                    # no embedding on purpose
        self.alice.put("fact", "note", "có nhúng", embedding=vec)

        self.assertEqual([h.key for h in self.alice.search(vec, kind="fact")], ["note"])
        self.assertEqual(self.alice.search(vec, kind="pref"), [])

    # -- compaction ------------------------------------------------------------------------
    def _archive(self, user_id: str) -> list[dict]:
        with self.store._conn() as conn:
            return [dict(r) for r in conn.execute(
                "SELECT kind, key, body FROM memory_archive WHERE user_id = %s ORDER BY key",
                (user_id,),
            ).fetchall()]

    def test_compaction_keeps_the_working_set_and_archives_the_rest(self) -> None:
        """The searchable set stays small; the facts stay somewhere."""
        for i in range(6):
            self.alice.put("fact", f"f{i}", f"giá trị {i}")

        report = self.alice.compact(keep=3)

        self.assertEqual(report["kinds"][0]["archived"], 3)
        self.assertEqual(self.alice.count("fact"), 3)
        self.assertEqual(len(self._archive("alice")), 3)
        # the three that stayed are the most recently written
        self.assertEqual(sorted(self.alice.get("fact", f"f{i}") for i in (3, 4, 5)),
                         ["giá trị 3", "giá trị 4", "giá trị 5"])

    def test_compaction_names_what_it_moved_instead_of_summarising_it(self) -> None:
        """A model summary of forty facts is a new claim with no source. The digest is JSON."""
        import json as _json
        for i in range(5):
            self.alice.put("fact", f"f{i}", f"giá trị {i}")

        self.alice.compact(keep=2)
        digest = self.alice.get("digest", "fact")
        self.assertIsNotNone(digest)
        parsed = _json.loads(digest)
        self.assertEqual(parsed["archived"], 3)
        self.assertEqual(parsed["keys"], ["f0", "f1", "f2"])
        self.assertLess(len(digest.encode()), mem.BODY_LIMIT)

    def test_running_compaction_twice_leaves_one_digest_row(self) -> None:
        for i in range(4):
            self.alice.put("fact", f"f{i}", str(i))
        self.alice.compact(keep=1)
        self.alice.compact(keep=1)

        self.assertEqual(self.alice.count("digest"), 1)

    def test_an_archived_fact_can_be_brought_back(self) -> None:
        self.alice.put("fact", "dau-11", "giảm 12%")
        for i in range(3):
            self.alice.put("fact", f"new{i}", str(i))
        self.alice.compact(keep=1)

        self.assertIsNone(self.alice.get("fact", "dau-11"))
        self.assertTrue(self.alice.restore("fact", "dau-11"))
        self.assertEqual(self.alice.get("fact", "dau-11"), "giảm 12%")
        # only the restored key leaves the archive; the others are still there, by design
        self.assertNotIn("dau-11", [r["key"] for r in self._archive("alice")])

    def test_compaction_never_touches_another_scope(self) -> None:
        for i in range(5):
            self.alice.put("fact", f"a{i}", str(i))
            self.bob.put("fact", f"b{i}", str(i))

        self.alice.compact(keep=1)

        self.assertEqual(self.bob.count("fact"), 5)
        self.assertEqual(self._archive("bob"), [])

    def test_a_scope_under_the_cap_is_left_alone(self) -> None:
        for i in range(3):
            self.alice.put("fact", f"f{i}", str(i))
        report = self.alice.compact(keep=10)
        self.assertEqual(report["kinds"][0]["archived"], 0)
        self.assertEqual(self.alice.count("fact"), 3)

    def test_compaction_refuses_to_be_deletion_in_disguise(self) -> None:
        with self.assertRaises(mem.MemoryError):
            self.alice.compact(keep=0)
        with self.assertRaises(mem.MemoryError):
            self.store.purge_archive(days=0)

    # -- the receipt -----------------------------------------------------------------------
    def _events(self) -> list[dict]:
        with self.store._conn() as conn:
            return [dict(r) for r in conn.execute(
                "SELECT actor_id, subject_id, op, rows FROM memory_event ORDER BY id"
            ).fetchall()]

    def test_reading_your_own_memory_leaves_a_receipt(self) -> None:
        """Traffic per person is a number; a read that leaves no trace is an assumption."""
        self.alice.put("fact", "a", "x", embedding=[1, 0, 0, 0, 0, 0, 0, 0])
        self.alice.get("fact", "a")
        self.alice.search([1, 0, 0, 0, 0, 0, 0, 0])

        events = [(e["actor_id"], e["op"], e["rows"]) for e in self._events()]
        self.assertIn(("alice", "get", 1), events)
        self.assertIn(("alice", "search", 1), events)

    def test_support_reading_someone_elses_memory_is_attributed_to_the_reader(self) -> None:
        """The read that needs a name later is the one where actor and subject differ."""
        self.bob.put("fact", "b", "bob's")
        self.alice.put("fact", "a", "alice's")

        report = self.alice and self.store.inspect("support-7", "bob")

        self.assertEqual(report["subject_id"], "bob")
        self.assertEqual(report["kinds"][0]["rows"], 1)
        cross = [e for e in self._events() if e["actor_id"] != e["subject_id"]]
        self.assertEqual([(e["actor_id"], e["subject_id"], e["op"]) for e in cross],
                         [("support-7", "bob", "inspect")])
        # shape, not content: the report never carries a body
        self.assertNotIn("body", str(report))

    def test_a_deletion_is_recorded(self) -> None:
        self.alice.put("fact", "a", "x")
        self.alice.forget(kind="fact")

        forgets = [e for e in self._events() if e["op"] == "forget"]
        self.assertEqual(len(forgets), 1)
        self.assertEqual(forgets[0]["rows"], 1)

    # -- the number someone will ask for ----------------------------------------------------
    def test_stats_names_where_the_bytes_are(self) -> None:
        self.alice.put("fact", "a", "x" * 10)
        self.bob.put("fact", "b", "y" * 100)

        rows = {r["kind"]: r for r in self.store.stats()}
        self.assertEqual(rows["fact"]["rows"], 2)
        self.assertEqual(rows["fact"]["body_bytes"], 110)


if __name__ == "__main__":
    unittest.main()
