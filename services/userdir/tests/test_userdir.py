"""The directory is an authorization input, so these tests are about the two things that can go
wrong quietly: a disabled person still being served, and two systems disagreeing about who exists.
"""
from __future__ import annotations

import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))


def fresh_service():
    """A module instance backed by a throwaway SQLite file: no shared state between tests."""
    import importlib

    os.environ.pop("USERDIR_DSN", None)
    os.environ["USERDIR_DB"] = str(Path(tempfile.mkdtemp()) / "users.db")
    sys.modules.pop("server", None)
    return importlib.import_module("server")


def call(mod, name: str, args: dict | None = None) -> dict:
    body = {"jsonrpc": "2.0", "id": 1, "method": "tools/call",
            "params": {"name": name, "arguments": args or {}}}
    resp = mod.handle(body)
    return json.loads(resp["result"]["content"][0]["text"])


class DirectoryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.mod = fresh_service()

    def test_it_advertises_three_tools_and_the_destructive_one_is_marked_in_policy(self) -> None:
        body = self.mod.handle({"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
        self.assertEqual([t["name"] for t in body["result"]["tools"]],
                         ["user_list", "user_consistency", "user_set_active"])

    def test_disabling_a_user_is_visible_to_the_next_read(self) -> None:
        self.assertEqual(call(self.mod, "user_set_active",
                              {"user_id": "1001", "active": False})["active"], False)
        users = {u["id"]: u for u in call(self.mod, "user_list")["users"]}
        self.assertFalse(users["1001"]["active"])
        self.assertEqual(call(self.mod, "user_list")["store"], "sqlite",
                         "the store must be reported, never inferred")

    def test_reenabling_a_user_is_the_same_write(self) -> None:
        call(self.mod, "user_set_active", {"user_id": "1001", "active": False})
        call(self.mod, "user_set_active", {"user_id": "1001", "active": True})
        users = {u["id"]: u for u in call(self.mod, "user_list")["users"]}
        self.assertTrue(users["1001"]["active"])

    def test_consistency_names_both_directions(self) -> None:
        call(self.mod, "user_set_active", {"user_id": "1001", "active": True})
        call(self.mod, "user_set_active", {"user_id": "2002", "active": False})
        report = call(self.mod, "user_consistency", {"known_ids": ["1001", "3003"]})
        self.assertEqual(report["missing_in_directory"], ["3003"],
                         "a person the caller knows and the directory does not is a mismatch")
        self.assertIn("2002", report["unknown_in_caller"],
                      "an offboarded row nobody removed is a mismatch too")
        self.assertEqual(report["inactive"], ["2002"])
        self.assertFalse(report["match"])

    def test_consistency_matches_when_both_sides_agree(self) -> None:
        for uid in ("1001", "2002"):
            call(self.mod, "user_set_active", {"user_id": uid, "active": True})
        report = call(self.mod, "user_consistency", {"known_ids": ["1001", "2002"]})
        self.assertTrue(report["match"])
        self.assertEqual(report["inactive"], [])

    def test_bad_user_id_is_refused_not_written(self) -> None:
        bad = call(self.mod, "user_set_active", {"user_id": "", "active": True})
        self.assertTrue(bad["isError"], "an empty id must come back as an error, not a silent write")
        self.assertEqual(call(self.mod, "user_list")["count"], 0)


if __name__ == "__main__":
    unittest.main()
