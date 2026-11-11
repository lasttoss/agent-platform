"""Adapter tests: real HTTP to the gateway, real HTTP to a fake Telegram API.

No real bot token is used, so these run in CI and never touch the user's bots.
"""
from __future__ import annotations

import importlib
import os
import sys
import tempfile
import threading
import uuid
import time
import unittest
import urllib.request
from http.server import ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "adapters"))
sys.path.insert(0, str(ROOT / "tests"))

from fake_telegram import FakeTelegram  # noqa: E402
from gateway import server as gw_server  # noqa: E402


def _start_gateway(state_dir: Path):
    # make_server() loads the policy and installs per-test state: no cache or quota bleed
    httpd = gw_server.make_server(port=0, state_dir=state_dir)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    return httpd


class AdapterTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.state = tempfile.mkdtemp()
        cls.gw = _start_gateway(Path(cls.state))
        cls.port = cls.gw.server_address[1]
        time.sleep(0.3)

    def setUp(self) -> None:
        self.fake = FakeTelegram()
        os.environ["TELEGRAM_API_BASE"] = self.fake.base
        os.environ["GATEWAY_URL"] = f"http://127.0.0.1:{self.port}"
        os.environ["OFFSET_FILE"] = str(Path(self.state) / f"offset-{uuid.uuid4().hex[:8]}.json")
        import telegram_poll
        self.tg = importlib.reload(telegram_poll)

    def tearDown(self) -> None:
        self.fake.stop()

    def _drain(self, token: str = "test") -> int:
        """One polling pass: fetch pending updates and handle them."""
        data = self.tg.api_call(token, "getUpdates", {"offset": self.tg.load_offset()})
        n = 0
        for update in data["result"]:
            self.tg.save_offset(update["update_id"] + 1)
            self.tg.handle_update(token, update)
            n += 1
        return n

    def test_dm_question_is_answered_in_the_same_chat(self):
        self.fake.push(1001, 1001, "private", "lương tháng này của tôi")
        self.assertEqual(self._drain(), 1)
        self.assertEqual(len(self.fake.sent), 1)
        self.assertEqual(self.fake.sent[0]["chat_id"], 1001)
        self.assertIn("12.345.000", self.fake.sent[0]["text"])

    def test_personal_answer_never_lands_in_the_group(self):
        self.fake.push(1001, -100123, "supergroup", "lương tháng này của tôi")
        self._drain()
        to_group = [m for m in self.fake.sent if m["chat_id"] == -100123]
        to_user = [m for m in self.fake.sent if m["chat_id"] == 1001]
        self.assertEqual(len(to_group), 1)
        self.assertNotIn("12.345.000", to_group[0]["text"])
        self.assertEqual(len(to_user), 1)
        self.assertIn("12.345.000", to_user[0]["text"])

    def test_unknown_user_gets_no_answer(self):
        self.fake.push(9999, 9999, "private", "tài liệu runbook")
        self._drain()
        self.assertEqual(len(self.fake.sent), 1)
        self.assertIn("chưa được cấp quyền", self.fake.sent[0]["text"])

    def test_offset_advances_so_updates_are_not_replayed(self):
        self.fake.push(1001, 1001, "private", "trạng thái")
        self.assertEqual(self._drain(), 1)
        self.assertEqual(self._drain(), 0)
        self.assertEqual(len(self.fake.sent), 1)

    def test_409_conflict_is_reported_not_looped(self):
        self.fake.conflict = True
        try:
            self.tg.api_call("test", "getUpdates", {})
            self.fail("expected HTTPError 409")
        except urllib.error.HTTPError as e:
            self.assertEqual(e.code, 409)

    def test_configuration_is_read_from_env_only(self):
        self.assertFalse(self.tg.API.endswith("api.telegram.org"))  # pointed at the fake
        self.assertIn(str(self.port), self.tg.GATEWAY)


if __name__ == "__main__":
    unittest.main(verbosity=2)
