"""The model service is tested for what it must NOT do: answer without a provider, or hand the
gateway a tool call it was never offered."""
from __future__ import annotations

import json
import os
import sys
import threading
import unittest
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import server  # noqa: E402


class ModelServiceTests(unittest.TestCase):
    def setUp(self) -> None:
        self._env = {k: os.environ.get(k) for k in ("MODEL_BASE_URL", "FAKE_TOOL_CALL")}
        os.environ.pop("MODEL_BASE_URL", None)
        os.environ.pop("FAKE_TOOL_CALL", None)

    def tearDown(self) -> None:
        for key, value in self._env.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value

    def test_without_a_provider_it_answers_nothing_rather_than_inventing(self) -> None:
        out = server.complete({"user": "lương tháng này của tôi", "tools": []})
        self.assertEqual(out["content"], "")
        self.assertEqual(out["tool_calls"], [])
        self.assertTrue(out["mock"])

    def test_tool_call_arguments_arrive_as_objects(self) -> None:
        os.environ["FAKE_TOOL_CALL"] = "docs_search"
        out = server.complete({"user": "tài liệu runbook", "tools": []})
        self.assertEqual(out["tool_calls"][0]["name"], "docs_search")
        self.assertIsInstance(out["tool_calls"][0]["arguments"], dict)

    def test_the_request_names_itself(self) -> None:
        """A provider refused the default urllib User-Agent with 403 (measured), so the service
        must send its own name — a client that hides what it is gets blocked."""
        seen = {}

        class Stub(BaseHTTPRequestHandler):
            def do_POST(self):
                seen["ua"] = self.headers.get("User-Agent", "")
                raw = json.dumps({"choices": [{"message": {"content": "hi"}}],
                                  "usage": {"prompt_tokens": 1, "completion_tokens": 1}}).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def log_message(self, *a):
                return

        httpd = ThreadingHTTPServer(("127.0.0.1", 0), Stub)
        threading.Thread(target=httpd.serve_forever, daemon=True).start()
        try:
            os.environ["MODEL_BASE_URL"] = f"http://127.0.0.1:{httpd.server_address[1]}"
            out = server.complete({"user": "hello"})
            self.assertFalse(out["mock"])
            self.assertIn("agent-platform", seen["ua"])
        finally:
            httpd.shutdown()
            os.environ.pop("MODEL_BASE_URL", None)

    def test_unknown_method_is_an_error_not_an_empty_result(self) -> None:
        body = server.handle({"jsonrpc": "2.0", "id": 7, "method": "tools/subscribe"})
        self.assertEqual(body["error"]["code"], -32601)


class ModelServiceHttpTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.srv = server.make_server(port=0)
        cls.port = cls.srv.server_address[1]
        import threading
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.base = f"http://127.0.0.1:{cls.port}"

    @classmethod
    def tearDownClass(cls) -> None:
        cls.srv.shutdown()

    def _rpc(self, method: str, params: dict | None = None) -> dict:
        req = urllib.request.Request(f"{self.base}/mcp",
                                     data=json.dumps({"jsonrpc": "2.0", "id": 1,
                                                      "method": method,
                                                      "params": params or {}}).encode(),
                                     headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=5) as resp:
            return json.loads(resp.read().decode())

    def test_tools_list_advertises_one_tool(self) -> None:
        body = self._rpc("tools/list")
        self.assertEqual([t["name"] for t in body["result"]["tools"]], ["chat_complete"])

    def test_tools_call_wraps_the_turn_in_json(self) -> None:
        body = self._rpc("tools/call", {"name": "chat_complete",
                                        "arguments": {"user": "xin chào"}})
        payload = json.loads(body["result"]["content"][0]["text"])
        self.assertIn("tool_calls", payload)
        self.assertFalse(body["result"]["isError"])


if __name__ == "__main__":
    unittest.main()
