"""A shared audit view is only worth having if a retry does not double the numbers and a rate
cannot be computed from the wrong denominator.
"""
from __future__ import annotations

import importlib
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))


def fresh():
    os.environ["AUDIT_DB"] = str(Path(tempfile.mkdtemp()) / "audit.db")
    sys.modules.pop("server", None)
    return importlib.import_module("server")


class SinkTests(unittest.TestCase):
    def setUp(self) -> None:
        self.mod = fresh()

    def test_it_stores_an_event_and_reports_it(self) -> None:
        accepted, dup = self.mod.STORE.add([{"ts": "2026-11-12T09:00:00+07:00", "user": "1001",
                                             "chat": "dm", "decision": "answered",
                                             "tool": "salary_self"}])
        self.assertEqual((accepted, dup), (1, 0))
        report = self.mod.STORE.report("2026-11-12")
        self.assertEqual(report["events"], 1)
        self.assertEqual(report["decisions"], {"answered": 1})
        self.assertEqual(report["shed_rate"], 0.0)

    def test_a_retried_delivery_is_not_a_second_event(self) -> None:
        event = {"event_id": "e1", "ts": "2026-11-12T09:00:00+07:00", "user": "1001",
                 "decision": "answered"}
        self.mod.STORE.add([event])
        accepted, dup = self.mod.STORE.add([event])
        self.assertEqual((accepted, dup), (0, 1))
        self.assertEqual(self.mod.STORE.report("2026-11-12")["events"], 1)

    def test_two_events_in_the_same_second_are_two_events(self) -> None:
        self.mod.STORE.add([
            {"event_id": "e1", "ts": "2026-11-12T09:00:00+07:00", "user": "1001",
             "decision": "answered", "tool": "status"},
            {"event_id": "e2", "ts": "2026-11-12T09:00:00+07:00", "user": "1001",
             "decision": "answered", "tool": "status"},
        ])
        self.assertEqual(self.mod.STORE.report("2026-11-12")["events"], 2,
                         "the id is the identity, not the content")

    def test_lines_without_an_id_fall_back_to_content_and_timestamp(self) -> None:
        self.mod.STORE.add([
            {"ts": "2026-11-12T09:00:00+07:00", "user": "1001", "decision": "answered"},
            {"ts": "2026-11-12T09:00:01+07:00", "user": "1001", "decision": "answered"},
        ])
        self.assertEqual(self.mod.STORE.report("2026-11-12")["events"], 2,
                         "old lines that differ only by time were collapsing into one")

    def test_the_same_event_tomorrow_is_a_new_event(self) -> None:
        today = {"ts": "2026-11-12T09:00:00+07:00", "user": "1001", "decision": "answered"}
        tomorrow = {"ts": "2026-11-13T09:00:00+07:00", "user": "1001", "decision": "answered"}
        self.mod.STORE.add([today])
        accepted, dup = self.mod.STORE.add([tomorrow])
        self.assertEqual((accepted, dup), (1, 0), "dedupe must not swallow a legitimate repeat")

    def test_rates_use_the_day_total(self) -> None:
        events = [{"event_id": "e%d" % i, "ts": "2026-11-12T09:00:0%d+07:00" % i,
                   "user": "1001", "decision": "answered" if i != 3 else "shed",
                   "tool": "ocr_extract"} for i in range(4)]
        self.mod.STORE.add(events)
        report = self.mod.STORE.report("2026-11-12")
        self.assertEqual(report["events"], 4)
        self.assertEqual(report["shed_rate"], 0.25)
        self.assertEqual(report["tools"], {"ocr_extract": 4})

    def test_denied_tools_count_toward_the_denied_rate(self) -> None:
        self.mod.STORE.add([
            {"event_id": "a", "ts": "2026-11-12T09:00:00+07:00", "user": "1001", "decision": "denied"},
            {"event_id": "b", "ts": "2026-11-12T09:00:01+07:00", "user": "1001", "decision": "tool_not_allowed"},
            {"event_id": "c", "ts": "2026-11-12T09:00:02+07:00", "user": "1001", "decision": "answered"},
            {"event_id": "d", "ts": "2026-11-12T09:00:03+07:00", "user": "1001", "decision": "answered"},
        ])
        self.assertEqual(self.mod.STORE.report("2026-11-12")["denied_rate"], 0.5)


    def test_a_run_comes_back_in_order_with_its_steps(self) -> None:
        store = self.mod.STORE
        store.add([
            {"event": "mcp_tools_list", "run_id": "r1", "user": "1001", "ts": "2026-11-19T10:00:00Z",
             "event_id": "e1"},
            {"event": "mcp_call", "run_id": "r1", "user": "1001", "tool": "echo", "cache": "hit",
             "ts": "2026-11-19T10:00:01Z", "event_id": "e2"},
            {"event": "mcp_call", "run_id": "r2", "user": "1001", "tool": "docs_search",
             "ts": "2026-11-19T10:00:02Z", "event_id": "e3"},
        ])
        out = store.run("r1")
        self.assertEqual(out["count"], 2, out)
        self.assertEqual([e["event"] for e in out["events"]], ["mcp_tools_list", "mcp_call"])
        self.assertEqual(out["events"][1]["cache"], "hit", out)

    def test_a_run_id_from_another_person_is_not_merged_in(self) -> None:
        """The run id joins steps; it never widens who may read them. Two runs stay two."""
        store = self.mod.STORE
        store.add([
            {"event": "mcp_call", "run_id": "r1", "user": "1001", "ts": "2026-11-19T10:00:00Z"},
            {"event": "mcp_call", "run_id": "r9", "user": "2002", "ts": "2026-11-19T10:00:01Z"},
        ])
        self.assertEqual(store.run("r1")["count"], 1)
        self.assertEqual(store.run("r9")["count"], 1)

    def test_an_unknown_run_is_empty_not_an_error(self) -> None:
        store = self.mod.STORE
        out = store.run("never-existed")
        self.assertEqual(out["count"], 0, out)
        self.assertEqual(out["events"], [])
if __name__ == "__main__":
    unittest.main()
