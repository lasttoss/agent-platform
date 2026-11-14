"""The service against a fake ClickHouse and a fake model service.

Both are real HTTP servers, so the tests exercise the same code path production does: request
shape, headers, error extraction, and the decision not to run anything a guard refused.
"""
import json
import pathlib
import sys
import threading
import unittest
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
import server  # noqa: E402


class FakeClickHouse:
    """Records every query and answers like ClickHouse HTTP would."""

    def __init__(self, fail_on=None, rows=None):
        self.queries = []
        self.params = []
        self.fail_on = fail_on
        self.rows = rows if rows is not None else [{"a": 1}, {"a": 2}]
        outer = self

        class H(BaseHTTPRequestHandler):
            def do_POST(self):
                size = int(self.headers.get("Content-Length", "0"))
                body = self.rfile.read(size).decode()
                query = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
                outer.queries.append(body)
                outer.params.append(query)
                if outer.fail_on and outer.fail_on in body:
                    raw = b"Code: 62. DB::Exception: Syntax error (at row 1)\n"
                    self.send_response(500)
                    self.send_header("Content-Length", str(len(raw)))
                    self.end_headers()
                    self.wfile.write(raw)
                    return
                if "system.columns" in body:
                    doc = {"data": [
                        {"table": "events", "name": "ts", "type": "DateTime"},
                        {"table": "events", "name": "player_id", "type": "UInt32"},
                        {"table": "players", "name": "id", "type": "UInt32"},
                    ]}
                else:
                    cap = int(query.get("max_result_rows", ["1000"])[0])
                    rows = outer.rows[:cap]
                    doc = {"data": rows, "rows": len(rows),
                           "rows_before_limit_at_least": len(outer.rows)}
                raw = json.dumps(doc).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def log_message(self, *a):
                return

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}"
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()

    def close(self):
        self.httpd.shutdown()


class FakeModel:
    """An MCP service answering `chat_complete`, like services/model-mcp does."""

    def __init__(self, content="", mock=False):
        self.content, self.mock, self.calls = content, mock, []
        outer = self

        class H(BaseHTTPRequestHandler):
            def do_POST(self):
                size = int(self.headers.get("Content-Length", "0"))
                body = json.loads(self.rfile.read(size) or b"{}")
                outer.calls.append(body)
                inner = json.dumps({"content": outer.content, "tool_calls": [],
                                    "mock": outer.mock,
                                    "usage": {"prompt_tokens": 0, "completion_tokens": 0}})
                raw = json.dumps({"jsonrpc": "2.0", "id": body.get("id"),
                                  "result": {"isError": False,
                                             "content": [{"type": "text", "text": inner}]}}).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def log_message(self, *a):
                return

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}/mcp"
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()

    def close(self):
        self.httpd.shutdown()


class ServiceTest(unittest.TestCase):
    def setUp(self):
        self.ch = FakeClickHouse()
        self._ch_url, self._model = server.CH_URL, server.MODEL_MCP_URL
        self._db = server.CH_DATABASE
        server.CH_URL, server.CH_DATABASE = self.ch.url, "analytics"

    def tearDown(self):
        server.CH_URL, server.MODEL_MCP_URL, server.CH_DATABASE = self._ch_url, self._model, self._db
        self.ch.close()

    def test_query_returns_rows_and_settings(self):
        out = server.handle({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                             "params": {"name": "ch_query", "arguments": {"sql": "SELECT a FROM t", "limit": 50}}})
        self.assertFalse(out["result"]["isError"])
        body = json.loads(out["result"]["content"][0]["text"])
        self.assertEqual(body["row_count"], 2)
        self.assertFalse(body["truncated"])
        sent = self.ch.params[0]
        # The read-only guarantee is the database user, not a per-query setting: sending
        # readonly=1 to a readonly=2 user is itself a refused settings change (Code 164).
        self.assertNotIn("readonly", sent)
        self.assertEqual(sent["max_result_rows"], ["50"])
        self.assertEqual(sent["database"], ["analytics"])
        self.assertEqual(sent["result_overflow_mode"], ["break"])

    def test_mutation_is_refused_before_any_connection(self):
        out = server.handle({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                             "params": {"name": "ch_query", "arguments": {"sql": "DROP TABLE events"}}})
        self.assertTrue(out["result"]["isError"])
        text = out["result"]["content"][0]["text"]
        self.assertIn("sql_rejected", text)
        self.assertEqual(self.ch.queries, [], "a refused query must not reach ClickHouse")

    def test_clickhouse_error_is_reported_not_swallowed(self):
        self.ch.fail_on = "SELECT broken"
        out = server.run_sql("SELECT broken FROM t")
        self.assertTrue(out["is_error"])
        self.assertIn("clickhouse:", out["error"])
        self.assertIn("Syntax error", out["error"])

    def test_schema_lists_tables(self):
        text = server.schema_text()
        self.assertRegex(text, r"events\n  ts\s+DateTime")
        self.assertRegex(text, r"  player_id\s+UInt32")
        self.assertRegex(text, r"players\n  id\s+UInt32")

    def test_rows_are_capped_and_said_so(self):
        self.ch.rows = [{"a": i} for i in range(10)]
        out = server.run_sql("SELECT a FROM t", limit=3)
        self.assertTrue(out["truncated"])
        self.assertEqual(out["row_count"], 3)

    def test_nlp_ask_runs_the_model_sql(self):
        model = FakeModel(content="```sql\nSELECT a FROM t LIMIT 1\n```")
        server.MODEL_MCP_URL = model.url
        try:
            out = server.handle({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                                 "params": {"name": "nlp_ask", "arguments": {"question": "how many rows"}}})
            body = json.loads(out["result"]["content"][0]["text"])
            self.assertEqual(body["sql"], "SELECT a FROM t LIMIT 1")
            self.assertEqual(body["row_count"], 2)
            self.assertEqual(body["question"], "how many rows")
            # The model was given the schema, not asked to invent table names.
            system = model.calls[0]["params"]["arguments"]["system"]
            self.assertIn("events", system)
            self.assertIn("player_id", system)
        finally:
            model.close()

    def test_nlp_ask_refuses_model_sql_that_writes(self):
        model = FakeModel(content="DELETE FROM events")
        server.MODEL_MCP_URL = model.url
        try:
            out = server.handle({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                                 "params": {"name": "nlp_ask", "arguments": {"question": "clean up"}}})
            self.assertTrue(out["result"]["isError"])
            self.assertIn("sql_rejected", out["result"]["content"][0]["text"])
            self.assertNotIn("DELETE", " ".join(self.ch.queries),
                             "a model must not be able to reach the database")
        finally:
            model.close()

    def test_nlp_ask_with_unconfigured_model_invents_nothing(self):
        model = FakeModel(content="", mock=True)
        server.MODEL_MCP_URL = model.url
        try:
            out = server.handle({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                                 "params": {"name": "nlp_ask", "arguments": {"question": "anything"}}})
            self.assertTrue(out["result"]["isError"])
            self.assertIn("model_returned_no_sql", out["result"]["content"][0]["text"])
            # Reading the schema for the prompt is expected; running a generated query is not.
            self.assertTrue(all("system.columns" in q for q in self.ch.queries), self.ch.queries)
        finally:
            model.close()

    def test_nlp_ask_reports_model_down(self):
        server.MODEL_MCP_URL = "http://127.0.0.1:1/mcp"
        out = server.ask({"question": "anything"})
        self.assertTrue(out["is_error"])
        self.assertIn("model_unavailable", out["error"])


if __name__ == "__main__":
    unittest.main()
