from __future__ import annotations

import sys
import tempfile
import time
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from gateway.cache import (CacheStore, exact_key, embed, cosine,  # noqa: E402
                           prefix_fingerprint)


class CacheTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.NamedTemporaryFile(suffix=".db", delete=False).name
        self.c = CacheStore(self.tmp)

    def test_exact_hit_returns_answer_and_counts(self):
        k = exact_key("lương tháng này", ["salary_self"], "v1", "dm", "user:1001")
        self.c.put_exact(k, "100.000 USD", ["salary_self"], 120, 60, "user:1001")
        hit = self.c.get_exact(k)
        self.assertEqual(hit["layer"], "exact")
        self.assertEqual(hit["answer"], "100.000 USD")
        self.assertEqual(self.c.stats()["l1_hit"], 1)

    def test_tool_order_does_not_change_the_key(self):
        a = exact_key("hỏi", ["a", "b"], "v1", "dm", "user:1")
        b = exact_key("hỏi", ["b", "a"], "v1", "dm", "user:1")
        self.assertEqual(a, b)

    def test_personal_scope_is_part_of_the_key(self):
        a = exact_key("lương", ["salary_self"], "v1", "dm", "user:1001")
        b = exact_key("lương", ["salary_self"], "v1", "dm", "user:2002")
        self.assertNotEqual(a, b)

    def test_ttl_expiry(self):
        k = exact_key("x", [], "v1", "dm", "user:1")
        self.c.put_exact(k, "old", [], 10, 1, "user:1")
        self.assertIsNotNone(self.c.get_exact(k))
        time.sleep(1.2)
        self.assertIsNone(self.c.get_exact(k))

    def test_ttl_zero_disables_caching(self):
        k = exact_key("salary", ["salary_self"], "v1", "dm", "user:1")
        self.c.put_exact(k, "secret", ["salary_self"], 100, 0, "user:1")
        self.assertIsNone(self.c.get_exact(k))

    def test_semantic_hit_for_reworded_question(self):
        self.c.put_semantic("luong thang nay cua toi bao nhieu", "100k USD", 200, 600, "user:1001")
        hit = self.c.get_semantic("thang nay luong cua toi bao nhieu", "user:1001")
        self.assertIsNotNone(hit)
        self.assertEqual(hit["layer"], "semantic")
        self.assertGreaterEqual(hit["similarity"], 0.93)

    def test_semantic_never_crosses_scope(self):
        self.c.put_semantic("luong thang nay cua toi", "100k USD", 200, 600, "user:1001")
        self.assertIsNone(self.c.get_semantic("luong thang nay cua toi", "user:2002"))

    def test_unrelated_question_misses(self):
        self.c.put_semantic("luong thang nay cua toi", "100k USD", 200, 600, "user:1001")
        self.assertIsNone(self.c.get_semantic("deploy production now", "user:1001"))

    def test_prefix_fingerprint_is_stable_and_sorted(self):
        a = prefix_fingerprint(["z", "a"], "v1")
        b = prefix_fingerprint(["a", "z"], "v1")
        c = prefix_fingerprint(["a", "z"], "v2")
        self.assertEqual(a, b)
        self.assertNotEqual(a, c)

    def test_embedding_is_normalised(self):
        v = embed("backend engineer game server")
        self.assertAlmostEqual(cosine(v, v), 1.0, places=6)


if __name__ == "__main__":
    unittest.main(verbosity=2)
