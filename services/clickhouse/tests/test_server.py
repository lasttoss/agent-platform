"""The service against a fake ClickHouse and a fake model service.

Both are real HTTP servers, so the tests exercise the same code path production does: request
shape, headers, error extraction, and the decision not to run anything a guard refused.
"""
import json
import pathlib
import time
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
                if "type LIKE 'AggregateFunction(%" in body:
                    doc = {"data": [{"name": "events", "type": "AggregateFunction(count)"},
                                    {"name": "total_ms", "type": "AggregateFunction(sum, UInt32)"}]}
                elif body.strip().startswith("EXPLAIN ESTIMATE"):
                    doc = {"data": [{"database": "analytics", "table": "events",
                                     "parts": 4, "rows": 1000, "marks": 10}]}
                elif "system.columns" in body:
                    doc = {"data": [
                        {"table": "events", "name": "ts", "type": "DateTime"},
                        {"table": "events", "name": "player_id", "type": "UInt32"},
                        {"table": "players", "name": "id", "type": "UInt32"},
                        {"table": "events_daily", "name": "day", "type": "Date"},
                        {"table": "events_daily", "name": "events", "type": "AggregateFunction(count)"},
                    ]}
                elif "system.tables" in body:
                    doc = {"data": [
                        {"name": "events", "engine": "MergeTree", "create_table_query": ""},
                        {"name": "events_daily", "engine": "AggregatingMergeTree", "create_table_query": ""},
                        {"name": "events_daily_mv", "engine": "MaterializedView",
                         "create_table_query": "CREATE MATERIALIZED VIEW analytics.events_daily_mv TO analytics.events_daily AS SELECT 1"},
                        {"name": "players", "engine": "MergeTree", "create_table_query": ""},
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
        self.queue = content if isinstance(content, list) else None
        outer = self

        class H(BaseHTTPRequestHandler):
            def do_POST(self):
                size = int(self.headers.get("Content-Length", "0"))
                body = json.loads(self.rfile.read(size) or b"{}")
                outer.calls.append(body)
                content = outer.content
                if outer.queue:
                    content = outer.queue[min(len(outer.calls) - 1, len(outer.queue) - 1)]
                inner = json.dumps({"content": content, "tool_calls": [],
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
        self.assertRegex(text, r"events \(MergeTree\)")
        self.assertRegex(text, r"  player_id\s+UInt32")
        self.assertRegex(text, r"players \(MergeTree\)")

    def test_schema_says_which_table_is_pre_aggregated(self):
        """A model that only sees columns sums the raw table and is confidently wrong once the
        raw table is trimmed, so the description has to carry the engine and the Merge rule."""
        text = server.schema_text()
        self.assertIn("events_daily (AggregatingMergeTree)", text)
        self.assertIn("events_daily_mv (MaterializedView → events_daily)", text)
        self.assertIn("→ countMerge(events)", text)
        self.assertIn("Prefer the aggregated table", text)

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
            self.assertTrue(all(("system.columns" in q or "system.tables" in q) for q in self.ch.queries),
                            self.ch.queries)
        finally:
            model.close()

    def test_nlp_ask_reports_model_down(self):
        server.MODEL_MCP_URL = "http://127.0.0.1:1/mcp"
        out = server.ask({"question": "anything"})
        self.assertTrue(out["is_error"])
        self.assertIn("model_unavailable", out["error"])


class ParamsTest(unittest.TestCase):
    """`{name:Type}` values travel as query parameters, never as part of the SQL text."""

    def setUp(self):
        self.ch = FakeClickHouse()
        self._saved = server.CH_URL
        server.CH_URL = self.ch.url
        # Warm the combinator map, so these tests record only the caller's own SQL.
        server._STATE_CACHE.update({"at": time.time(), "map": {"events": "countMerge"}})

    def tearDown(self):
        server.CH_URL = self._saved
        server._STATE_CACHE.update({"at": 0.0, "map": {}})
        self.ch.httpd.shutdown()

    def test_values_are_sent_as_parameters_not_spliced_into_the_sql(self):
        out = server.run_sql("SELECT count() FROM events WHERE event = {ev:String}",
                             params={"ev": "level_fail"})
        self.assertFalse(out.get("is_error"), out)
        self.assertNotIn("level_fail", self.ch.queries[-1])
        self.assertEqual(self.ch.params[-1].get("param_ev"), ["level_fail"])

    def test_a_value_that_looks_like_sql_stays_a_value(self):
        hostile = "x'; DROP TABLE events; --"
        out = server.run_sql("SELECT {v:String} AS echoed", params={"v": hostile})
        self.assertFalse(out.get("is_error"), out)
        self.assertEqual(self.ch.params[-1].get("param_v"), [hostile])

    def test_a_missing_parameter_is_named_before_the_query_is_sent(self):
        out = server.run_sql("SELECT count() FROM events WHERE event = {ev:String}")
        self.assertIn("missing param ev", out.get("error", ""))
        self.assertEqual(self.ch.queries, [])

    def test_an_unknown_parameter_is_refused(self):
        out = server.run_sql("SELECT count() FROM events WHERE event = {ev:String}",
                             params={"ev": "a", "extra": "b"})
        self.assertIn("unknown param extra", out.get("error", ""))


class LintTest(unittest.TestCase):
    """A combinator reads a state; wrapping it in an aggregate is refused with the fix."""

    def setUp(self):
        self.ch = FakeClickHouse()
        self._saved = server.CH_URL
        server.CH_URL = self.ch.url
        server._STATE_CACHE.update({"at": 0.0, "map": {}})

    def tearDown(self):
        server.CH_URL = self._saved
        server._STATE_CACHE.update({"at": 0.0, "map": {}})
        self.ch.httpd.shutdown()

    def test_aggregate_over_a_state_is_refused_locally(self):
        out = server.run_sql("SELECT sum(countMerge(events)) FROM events_daily")
        self.assertIn("aggregate over an aggregate state", out.get("error", ""))
        self.assertIn("countMerge(events)", out.get("error", ""))
        self.assertEqual(self.ch.queries, [])  # nothing reached the database

    def test_the_right_combinator_on_the_wrong_state_is_refused_with_the_fix(self):
        """`sumMerge(events)` on a count state is what ClickHouse reports as "sum requires single
        argument"; the column types come from the database, so the refusal can name the fix."""
        out = server.run_sql("SELECT sumMerge(events) FROM events_daily")
        self.assertIn("wrong combinator", out.get("error", ""))
        self.assertIn("countMerge(events)", out.get("error", ""))
        self.assertFalse(any("sumMerge" in q for q in self.ch.queries))

    def test_the_right_combinator_passes(self):
        out = server.run_sql("SELECT countMerge(events) FROM events_daily")
        self.assertFalse(out.get("is_error"), out)

    def test_merge_on_its_own_is_allowed(self):
        out = server.run_sql("SELECT countMerge(events) FROM events_daily")
        self.assertFalse(out.get("is_error"), out)

    def test_explain_estimate_reports_the_scan_without_running_it(self):
        out = server.explain("SELECT event FROM events GROUP BY event")
        self.assertFalse(out.get("is_error"), out)
        self.assertIn("EXPLAIN ESTIMATE", self.ch.queries[-1])
        self.assertEqual(out["rows_expected"], 1000)
        self.assertEqual(out["estimate"][0]["table"], "events")


class RepairTest(unittest.TestCase):
    """One rejected query earns exactly one corrected attempt, then it is an error."""

    def setUp(self):
        self.ch = FakeClickHouse()
        self._saved_ch = server.CH_URL
        server.CH_URL = self.ch.url
        self.model = FakeModel(content=[
            "SELECT sum(countMerge(events)) FROM events_daily",
            "SELECT countMerge(events) FROM events_daily",
        ])
        self._saved_model = server.MODEL_MCP_URL
        server.MODEL_MCP_URL = self.model.url

    def tearDown(self):
        server.CH_URL = self._saved_ch
        server.MODEL_MCP_URL = self._saved_model
        self.model.close()
        self.ch.httpd.shutdown()

    def test_the_second_attempt_is_the_one_that_runs(self):
        out = server.ask({"question": "how many events"})
        self.assertFalse(out.get("is_error"), out)
        self.assertTrue(out.get("repaired"))
        self.assertIn("sum(countMerge", out.get("rejected_sql", ""))
        self.assertEqual(len(self.model.calls), 2)
        self.assertIn("rejected by the validator", self.model.calls[1]["params"]["arguments"]["user"])

    def test_a_model_that_repeats_itself_ends_as_an_error_not_a_loop(self):
        self.model.queue = ["SELECT sum(countMerge(events)) FROM events_daily"]
        out = server.ask({"question": "how many events"})
        self.assertTrue(out.get("is_error"))
        self.assertIn("aggregate over an aggregate state", out.get("error", ""))
        self.assertEqual(len(self.model.calls), 2)  # two, not three


if __name__ == "__main__":
    unittest.main()
