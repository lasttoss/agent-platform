"""Minimal fake of the Telegram Bot API, enough for the adapter's tests.

Implements getMe, getUpdates (cursor based) and sendMessage (records sent messages).
Used so the adapter's real network path can be tested without a real bot token.
"""
from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer


class FakeTelegram:
    def __init__(self) -> None:
        self.updates: list[dict] = []
        self.sent: list[dict] = []
        self.conflict = False
        fake = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):  # silence
                pass

            def _json(self, code: int, obj: dict) -> None:
                raw = json.dumps(obj).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_POST(self):
                length = int(self.headers.get("Content-Length") or 0)
                payload = json.loads(self.rfile.read(length) or b"{}")
                method = self.path.rsplit("/", 1)[-1]
                if method == "getMe":
                    return self._json(200, {"ok": True, "result": {"id": 1, "username": "test_bot"}})
                if method == "getUpdates":
                    if fake.conflict:
                        return self._json(409, {"ok": False, "description": "Conflict: terminated by other getUpdates"})
                    cursor = int(payload.get("offset", 0))
                    rest = [u for u in fake.updates if u["update_id"] >= cursor]
                    return self._json(200, {"ok": True, "result": rest})
                if method == "sendMessage":
                    fake.sent.append(payload)
                    return self._json(200, {"ok": True, "result": {"message_id": len(fake.sent)}})
                return self._json(404, {"ok": False, "description": "not found"})

        self.server = HTTPServer(("127.0.0.1", 0), Handler)
        self.port = self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    @property
    def base(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    def push(self, user_id: int, chat_id: int, chat_type: str, text: str) -> None:
        self.updates.append({
            "update_id": (self.updates[-1]["update_id"] + 1) if self.updates else 1,
            "message": {"from": {"id": user_id}, "chat": {"id": chat_id, "type": chat_type}, "text": text},
        })

    def stop(self) -> None:
        self.server.shutdown()
