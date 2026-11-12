"""MCP is the only door the gateway has. These tests pin the two things that matter about a
door: it advertises exactly one tool, and a bad path comes back as an error the caller can
read — not as a 200 with empty text."""
from __future__ import annotations

import json
import sys
import threading
import unittest
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import mcp  # noqa: E402
import server as ocr  # noqa: E402

HERE = Path(__file__).resolve().parent.parent
SAMPLES = HERE / "samples"


def _ensure_sample() -> Path:
    SAMPLES.mkdir(exist_ok=True)
    path = SAMPLES / "text.pdf"
    if not path.exists():
        sys.path.insert(0, str(SAMPLES))
        import make_samples
        make_samples.main()
    return path


class McpAdapterTests(unittest.TestCase):
    def setUp(self) -> None:
        self.pdf = _ensure_sample()

    def call(self, name: str, arguments: dict) -> dict:
        return mcp.handle({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                           "params": {"name": name, "arguments": arguments}}, ocr.run)

    def test_it_advertises_the_submit_and_read_pair(self) -> None:
        body = mcp.handle({"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, ocr.run)
        self.assertEqual([t["name"] for t in body["result"]["tools"]],
                         ["ocr_extract", "ocr_result"],
                         "extraction is slow: submit and read are two tools, not one blocking call")
        self.assertIn("path", body["result"]["tools"][0]["inputSchema"]["required"])

    def test_a_text_pdf_comes_back_as_a_json_document(self) -> None:
        body = self.call("ocr_extract", {"path": str(self.pdf.relative_to(HERE))})
        self.assertFalse(body["result"]["isError"])
        payload = json.loads(body["result"]["content"][0]["text"])
        # The engine is whatever this machine has: poppler when present, pypdf as fallback.
        # Asserting one of them by name is how a test starts lying on CI.
        self.assertIn(payload["engine"], {"pdftotext", "pypdf"})
        self.assertTrue(payload["text"].strip())
        self.assertIsInstance(payload["pages"], int)

    def test_a_path_outside_the_root_is_an_error_not_empty_text(self) -> None:
        body = self.call("ocr_extract", {"path": "../../../etc/passwd"})
        self.assertTrue(body["result"]["isError"])
        self.assertIn("error", json.loads(body["result"]["content"][0]["text"]))

    def test_an_unknown_tool_is_refused(self) -> None:
        body = self.call("deploy_prod", {})
        self.assertTrue(body["result"]["isError"])

    def test_unknown_method_returns_jsonrpc_error(self) -> None:
        body = mcp.handle({"jsonrpc": "2.0", "id": 3, "method": "resources/list"}, ocr.run)
        self.assertEqual(body["error"]["code"], -32601)


class McpOverHttpTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.pdf = _ensure_sample()
        cls.srv = ocr.make_server(port=0, root=str(HERE))
        cls.port = cls.srv.server_address[1]
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls) -> None:
        cls.srv.shutdown()

    def _post(self, body: dict) -> dict:
        req = urllib.request.Request(f"http://127.0.0.1:{self.port}/mcp",
                                     data=json.dumps(body).encode(),
                                     headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=30) as resp:
            return json.loads(resp.read().decode())

    def test_the_service_speaks_mcp_over_http(self) -> None:
        listed = self._post({"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
        self.assertEqual(listed["result"]["tools"][0]["name"], "ocr_extract")
        called = self._post({"jsonrpc": "2.0", "id": 2, "method": "tools/call",
                             "params": {"name": "ocr_extract",
                                        "arguments": {"path": "samples/text.pdf"}}})
        self.assertFalse(called["result"]["isError"])


if __name__ == "__main__":
    unittest.main()
