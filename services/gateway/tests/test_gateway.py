"""Policy tests. These run without a model key and without network access."""
from __future__ import annotations

import copy
import json
import os
import re
import sys
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from gateway import policy, server  # noqa: E402

ROOT = Path(__file__).resolve().parent.parent


def _post(port: int, payload: dict) -> tuple[int, dict]:
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}/v1/respond",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode())


def _start_fake_model(tool_calls=None, content="MODEL-ANSWER") -> dict:
    """A counting stand-in for an OpenAI-compatible endpoint: proves whether the gateway
    decided to spend tokens, without any key or network."""
    import json as _json
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

    state = {"count": 0, "last_request": {}}

    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_POST(self):
            state["count"] += 1
            state["last_request"] = _json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
            message = {"content": content}
            if tool_calls:
                message = {"content": None,
                           "tool_calls": [{"id": f"call_{i}", "type": "function",
                                           "function": {"name": c["name"],
                                                        "arguments": _json.dumps(c.get("arguments", {}))}}
                                          for i, c in enumerate(tool_calls)]}
            raw = _json.dumps({"choices": [{"message": message}],
                               "usage": {"prompt_tokens": 11, "completion_tokens": 7}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

    srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    state["port"] = srv.server_address[1]
    return state


class GatewayTest(unittest.TestCase):
    policy_path = ROOT / "config" / "policy.json"

    def setUp(self) -> None:
        # Deterministic in tests: never talk to a real OCR worker.
        os.environ["OCR_SERVICE_URL"] = "http://127.0.0.1:9"
        self.state = tempfile.mkdtemp()
        server.install_state(self.state)
        self.srv = server.make_server(str(self.policy_path), port=0)
        self.port = self.srv.server_address[1]
        self.t = threading.Thread(target=self.srv.serve_forever, daemon=True)
        self.t.start()

    def tearDown(self) -> None:
        self.srv.shutdown()
        self.srv.server_close()

    def ask(self, text: str, user="1001", chat="dm"):
        return _post(self.port, {"platform_user_id": user, "chat_type": chat, "text": text})

    def test_state_dir_env_is_honoured(self):
        import tempfile as _tf
        d = _tf.mkdtemp()
        os.environ["STATE_DIR"] = d
        try:
            srv = server.make_server(port=0)
            srv.server_close()
            self.assertEqual(str(server.CACHE.path), str(Path(d) / "cache.db"))
        finally:
            os.environ.pop("STATE_DIR", None)
            server.install_state(self.state)

    def test_group_never_receives_ungrounded_model_text(self):
        """With tool calling the model IS asked (it has to be, to pick a tool), so the rule is
        not "never call the model" but "never let its unsupported prose reach a group"."""
        calls = _start_fake_model()
        os.environ["MODEL_BASE_URL"] = f"http://127.0.0.1:{calls['port']}/v1"
        try:
            code, body = self.ask("thời tiết hôm nay thế nào", chat="group_internal")
            self.assertEqual(code, 200)
            self.assertEqual(body["chat_reply"], "I have no tool for that.")
            self.assertNotIn("MODEL-ANSWER", body["chat_reply"])
            self.assertEqual(calls["count"], 1)          # one turn, tools offered, prose discarded
        finally:
            os.environ.pop("MODEL_BASE_URL", None)

    def test_model_can_only_call_tools_it_was_offered(self):
        """The load-bearing test: the model asks for a destructive tool, the gateway refuses,
        the tool never executes, and the request never contained it in the first place."""
        calls = _start_fake_model(tool_calls=[{"name": "deploy_prod", "arguments": {}}])
        os.environ["MODEL_BASE_URL"] = f"http://127.0.0.1:{calls['port']}/v1"
        executed = []
        real_call = server.tools.call
        server.tools.call = lambda name, principal, args: executed.append(name) or real_call(name, principal, args)
        try:
            code, body = self.ask("deploy production now", chat="dm")
            self.assertEqual(code, 200)
            self.assertEqual(body["denied_tool"], "deploy_prod")
            self.assertIn("permission", body["chat_reply"])
            self.assertEqual(executed, [], "a destructive tool was executed")
            offered = {t["function"]["name"] for t in calls["last_request"]["tools"]}
            self.assertNotIn("deploy_prod", offered, "deploy_prod was offered to the model")
            self.assertIn("docs_search", offered)
        finally:
            server.tools.call = real_call
            os.environ.pop("MODEL_BASE_URL", None)

    def test_allowed_tool_call_executes(self):
        calls = _start_fake_model(tool_calls=[{"name": "docs_search", "arguments": {"q": "runbook"}}])
        os.environ["MODEL_BASE_URL"] = f"http://127.0.0.1:{calls['port']}/v1"
        try:
            code, body = self.ask("tìm giúp tôi runbook", chat="dm")
            self.assertEqual(code, 200)
            self.assertIn("runbook-restart", body["chat_reply"])
        finally:
            os.environ.pop("MODEL_BASE_URL", None)

    def test_dm_free_chat_does_call_the_model_when_opted_in(self):
        """The other half of the same rule: where the policy opts in, the model is used —
        otherwise the previous test would pass for the wrong reason."""
        calls = _start_fake_model()
        os.environ["MODEL_BASE_URL"] = f"http://127.0.0.1:{calls['port']}/v1"
        try:
            code, body = self.ask("thời tiết hôm nay thế nào", chat="dm")
            self.assertEqual(code, 200)
            self.assertEqual(body["chat_reply"], "MODEL-ANSWER")
            self.assertEqual(calls["count"], 1)
            self.assertEqual(body["usage"]["prompt_tokens"], 11)
        finally:
            os.environ.pop("MODEL_BASE_URL", None)

    def test_llm_proxy_base_url_with_path_prefix(self):
        """LiteLLM and friends expose OpenAI-compatible endpoints under /v1 on their own port.
        The gateway must build <base>/chat/completions and not mangle a trailing slash."""
        import json as _json
        from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

        seen = {}

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                seen["path"] = self.path
                seen["auth"] = self.headers.get("Authorization", "")
                self.rfile.read(int(self.headers.get("Content-Length") or 0))
                raw = _json.dumps({"choices": [{"message": {"content": "PROXY-ANSWER"}}],
                                   "usage": {"prompt_tokens": 3, "completion_tokens": 2}}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

        srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        port = srv.server_address[1]
        os.environ["MODEL_BASE_URL"] = f"http://127.0.0.1:{port}/v1/"     # trailing slash on purpose
        os.environ["MODEL_API_KEY"] = "sk-test"
        try:
            code, body = self.ask("thời tiết hôm nay thế nào", chat="dm")
            self.assertEqual(code, 200)
            self.assertEqual(body["chat_reply"], "PROXY-ANSWER")
            self.assertEqual(seen["path"], "/v1/chat/completions")
            self.assertTrue(seen["auth"].startswith("Bearer "))
        finally:
            os.environ.pop("MODEL_BASE_URL", None)
            os.environ.pop("MODEL_API_KEY", None)

    def test_free_chat_is_opt_in_per_chat(self):
        rules = policy.load_rules(self.policy_path)
        dm = policy.resolve(rules, "1001", "dm")
        group = policy.resolve(rules, "1001", "group_internal")
        self.assertTrue(policy.free_chat_enabled(rules, dm))
        self.assertFalse(policy.free_chat_enabled(rules, group))
        # a policy that never mentions the key must behave like "off"
        rules["chats"]["dm"].pop("free_chat", None)
        self.assertFalse(policy.free_chat_enabled(rules, dm))

    def test_committed_policy_contains_no_real_person(self):
        """Guard: repo templates hold demo users only. Real ids are 8-9 digits, demo ids 4.
        The real value is deliberately not written here — a guard must not leak what it guards."""
        for name in ("config/policy.json", "../../config/consent.json"):
            data = json.loads((Path(__file__).resolve().parent.parent / name).read_text(encoding="utf-8"))
            keys = data.get("users", data)
            for uid in keys:
                self.assertLessEqual(len(str(uid)), 4, f"a real-looking id is committed in {name}")
        # Money literals in tools.py must be the two counting-pattern fixtures and nothing else:
        # a plausible-looking amount is how real salary data ends up in a repo.
        tools_src = (Path(__file__).resolve().parent.parent / "gateway" / "tools.py").read_text(encoding="utf-8")
        amounts = set(re.findall(r"\b\d{1,3}(?:_\d{3}){2}\b", tools_src))
        self.assertEqual(amounts, {"12_345_000", "9_876_000"}, f"unexpected money literal: {amounts}")

    def test_local_override_merges_without_touching_the_template(self):
        rules = policy.load_rules(self.policy_path)
        consent = policy.load_consent(ROOT.parent.parent / "config" / "consent.json")
        local = {"users": {"424242": {"name": "Local", "role": "owner", "workspace": "x"}},
                 "consent": {"424242": {"local_ocr": []}}}
        rules, consent = policy.apply_local(rules, consent, local)
        self.assertIn("424242", rules["users"])
        self.assertEqual(consent["424242"], {"local_ocr": []})

    def test_unknown_user_is_rejected(self):
        code, body = self.ask("lương của tôi", user="9999")
        self.assertEqual(code, 403)
        self.assertEqual(body["denied"]["code"], "unknown_user")

    def test_dm_gets_personal_answer_in_chat(self):
        code, body = self.ask("lương tháng này của tôi bao nhiêu")
        self.assertEqual(code, 200)
        self.assertEqual(body["delivered"], "chat")
        self.assertIn("12.345.000 VND", body["chat_reply"])
        self.assertIsNone(body["dm"])

    def test_group_never_gets_personal_number(self):
        code, body = self.ask("lương tháng này của tôi bao nhiêu", chat="group_internal")
        self.assertEqual(code, 200)
        self.assertEqual(body["delivered"], "dm")
        self.assertNotIn("12.345.000 VND", body["chat_reply"])
        self.assertIn("12.345.000 VND", body["dm"]["text"])

    def test_public_group_has_no_personal_tool_visible(self):
        code, body = self.ask("lương tháng này của tôi bao nhiêu", chat="group_public")
        self.assertEqual(code, 200)
        self.assertNotIn("salary_self", body["tools_visible"])
        self.assertNotIn("12.345.000 VND", json.dumps(body, ensure_ascii=False))

    def test_destructive_tool_is_never_visible(self):
        for chat in ("dm", "group_internal", "group_public"):
            code, body = self.ask("deploy prod", chat=chat)
            self.assertNotIn("deploy_prod", body.get("tools_visible", []))

    def test_other_users_salary_cannot_be_requested(self):
        # Subject is the caller, so user 2002 asking gets 2002's number.
        code, body = self.ask("lương của Dũng", user="2002")
        self.assertEqual(code, 200)
        self.assertIn("9.876.000 VND", body["chat_reply"])
        self.assertNotIn("12.345.000 VND", body["chat_reply"])

    def test_quota_is_enforced(self):
        rules = json.loads(self.policy_path.read_text())
        rules["quota"]["requests_per_day"] = 1
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
            json.dump(rules, fh)
            tmp = fh.name
        self.srv.shutdown(); self.srv.server_close()
        (ROOT / "state" / "usage.json").unlink(missing_ok=True)
        self.srv = server.make_server(tmp, port=0)
        self.port = self.srv.server_address[1]
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

        self.assertEqual(self.ask("trạng thái")[0], 200)
        code, body = self.ask("trạng thái")
        self.assertEqual(code, 429)
        self.assertEqual(body["denied"]["code"], "quota_exceeded")

    def test_ocr_tool_is_exposed_in_dm_only(self):
        _, dm = self.ask("trạng thái")
        self.assertIn("ocr_extract", dm["tools_visible"])
        _, public = self.ask("trạng thái", chat="group_public")
        self.assertNotIn("ocr_extract", public["tools_visible"])

    def test_ocr_worker_down_is_a_clean_failure(self):
        code, body = self.ask("ocr samples/text.pdf")
        self.assertEqual(code, 503)
        self.assertEqual(body["denied"]["code"], "service_unavailable")
        self.assertIn("ocr_unavailable", body["denied"]["reason"])

    def test_ocr_request_in_group_goes_private(self):
        code, body = self.ask("ocr samples/text.pdf", chat="group_internal")
        # worker is unreachable in tests, so the failure surfaces, not the content
        self.assertEqual(code, 503)
        self.assertNotIn("text", json.dumps(body))

    def test_no_diacritics_input_is_understood(self):
        code, body = self.ask("luong thang nay cua toi")
        self.assertEqual(code, 200)
        self.assertIn("12.345.000 VND", body["chat_reply"])
        code, body = self.ask("trang thai")
        self.assertEqual(code, 200)
        self.assertIn("game=ok", body["chat_reply"])

    def test_no_diacritics_and_accents_share_one_cache_entry(self):
        self.ask("tài liệu runbook")
        _, body = self.ask("tai lieu runbook")
        self.assertEqual(body["cache"]["layer"], "exact")

    def test_second_identical_question_comes_from_cache(self):
        first = self.ask("tài liệu runbook")
        self.assertEqual(first[0], 200)
        second = self.ask("tài liệu runbook")
        self.assertEqual(second[0], 200)
        self.assertEqual(second[1]["cache"]["layer"], "exact")
        self.assertEqual(first[1]["chat_reply"], second[1]["chat_reply"])

    def test_salary_is_never_cached(self):
        self.ask("lương tháng này của tôi")
        again, body = self.ask("lương tháng này của tôi")
        self.assertNotIn("cache", body)

    def test_cache_does_not_leak_between_users(self):
        self.ask("lương tháng này của tôi", user="1001")
        _, b = self.ask("lương tháng này của tôi", user="2002")
        self.assertIn("9.876.000 VND", b["chat_reply"])
        self.assertNotIn("12.345.000 VND", b["chat_reply"])

    def test_consent_gates_provider_tools(self):
        # 1001 granted drive + imap; 2002 granted neither.
        _, granted = self.ask("trạng thái", user="1001")
        self.assertIn("drive_search", granted["tools_visible"])
        self.assertIn("mail_list", granted["tools_visible"])

        _, not_granted = self.ask("trạng thái", user="2002")
        self.assertNotIn("drive_search", not_granted["tools_visible"])
        self.assertNotIn("mail_list", not_granted["tools_visible"])
        self.assertIn("docs_search", not_granted["tools_visible"])  # no provider, no consent needed

    def test_malformed_policy_fails_closed(self):
        with self.assertRaises(policy.Denied):
            policy.load_rules(self._tmp_json({"users": {}}))

    def _tmp_json(self, obj: dict) -> str:
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
            json.dump(obj, fh)
            return fh.name


if __name__ == "__main__":
    unittest.main(verbosity=2)


class RoleTests(unittest.TestCase):
    """`role` was parsed and ignored, so "guest" had owner powers. A role may only narrow."""

    def setUp(self) -> None:
        self.rules = policy.load_rules(ROOT / "config" / "policy.json")

    def _rules_with_roles(self) -> dict:
        rules = copy.deepcopy(self.rules)
        rules["roles"] = {"intern": {"tools": ["docs_search"]}}
        rules["users"]["4004"] = {"name": "Intern", "role": "intern", "workspace": "x"}
        return rules

    def test_a_role_narrows_the_chat_allowlist(self) -> None:
        rules = self._rules_with_roles()
        principal = policy.resolve(rules, "4004", "dm")
        visible = policy.visible_tools(rules, principal)
        self.assertEqual(visible, ["docs_search"])
        with self.assertRaises(policy.Denied) as ctx:
            policy.decide(rules, principal, "salary_self")
        self.assertEqual(ctx.exception.code, "tool_not_in_role")

    def test_an_undefined_role_fails_the_load(self) -> None:
        rules = copy.deepcopy(self.rules)
        rules["users"]["5005"] = {"name": "Ghost", "role": "nobody", "workspace": "x"}
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
            json.dump(rules, fh)
        try:
            with self.assertRaises(policy.Denied) as ctx:
                policy.load_rules(fh.name)
            self.assertEqual(ctx.exception.code, "policy_invalid")
        finally:
            Path(fh.name).unlink()
