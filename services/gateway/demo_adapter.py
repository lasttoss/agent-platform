"""Offline end-to-end demo of the adapter chain, using a fake Telegram API.

Shows exactly what the adapter does with four kinds of update, without touching a real
bot token: dm, personal question in a group, unknown user, and a repeated question.
"""
from __future__ import annotations

import json
import os
import sys
import tempfile
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent
sys.path[:0] = [str(ROOT), str(ROOT / "adapters"), str(ROOT / "tests")]

from fake_telegram import FakeTelegram            # noqa: E402
from gateway import server as gw                  # noqa: E402


def main() -> None:
    state = Path(tempfile.mkdtemp())
    srv = gw.make_server(port=0, state_dir=state)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    port = srv.server_address[1]

    fake = FakeTelegram()
    os.environ.update(TELEGRAM_API_BASE=fake.base,
                      GATEWAY_URL=f"http://127.0.0.1:{port}",
                      OFFSET_FILE=str(state / "offset.json"))
    import telegram_poll as tg

    fake.push(1001, 1001, "private", "lương tháng này của tôi")
    fake.push(1001, 1001, "private", "lương tháng này của tôi")     # cache hit
    fake.push(1001, -100123, "supergroup", "lương tháng này của tôi")
    fake.push(9999, 9999, "private", "tài liệu runbook")

    for update in tg.api_call("t", "getUpdates", {"offset": 0})["result"]:
        result = tg.handle_update("t", update)
        print("update →", json.dumps(result, ensure_ascii=False))

    print("\ntelegram received:")
    for msg in fake.sent:
        print(f"  chat {msg['chat_id']:>8}  {msg['text']}")
    print("\ncache:", json.dumps(gw.CACHE.stats(), ensure_ascii=False))
    print("audit:")
    for line in (state / "audit.log").read_text().splitlines()[-4:]:
        print("  " + line)
    time.sleep(0.1)


if __name__ == "__main__":
    main()
