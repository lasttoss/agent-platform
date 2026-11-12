"""Minimal gateway: HTTP in, policy decision, tool call, optional model polish.

Design rules enforced here:
  * The caller identity comes from the request body ONLY in a deployment where the
    transport already authenticated it (Telegram/Discord gateway). Never from text.
  * Tools are filtered before the model sees them.
  * Personal answers in a group are delivered privately, never posted.
  * Every decision is written to an append-only audit log.
"""
from __future__ import annotations

import json
import os
import re
import threading
import time
from datetime import date
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from . import cache, model, policy, tools
from .tools import ToolError, call as tool_call

ROOT = Path(__file__).resolve().parent.parent
STATE = ROOT / "state"
STATE.mkdir(exist_ok=True)
AUDIT = STATE / "audit.log"
USAGE = STATE / "usage.json"
CACHE = cache.CacheStore(STATE / "cache.db")
POLICY_VERSION = os.environ.get("POLICY_VERSION", "v1")

# Cache TTL by tool. 0 means "never cache": the answer is either personal data that
# changes, or a live status that would be wrong to serve twice.
SYSTEM = ("You are a gateway assistant. Use a tool whenever one fits. Never invent numbers, "
          "names or status: if no tool returns them, say you do not have that information.")
TTL = {"salary_self": 0, "mail_list": 0, "status": 30, "docs_search": 3600,
       "ocr_extract": 86400, "drive_search": 300, "jira_search": 600}
_LOCK = threading.Lock()

INTENT = [
    (("lương", "salary", "payroll"), "salary_self", {}),
    (("tài liệu", "docs", "runbook", "search"), "docs_search", {"q": "runbook"}),
    (("trạng thái", "status", "health"), "status", {}),
]
OCR_HINT = ("ocr", "trích xuất", "đọc file", ".pdf", ".png", ".jpg", ".jpeg")

# Match on folded text, so the intent keys must be folded too: "lương" == "luong".
INTENT = [(tuple(cache.fold(k) for k in keys), name, args) for keys, name, args in INTENT]
OCR_HINT = tuple(cache.fold(k) for k in OCR_HINT)
PATH_RE = re.compile(r"[\w./\-]+\.(?:pdf|png|jpe?g|tiff?|bmp|webp)", re.I)


def install_state(directory: str | Path) -> None:
    """Point the gateway at a state directory (usage, audit, cache).

    Tests and side-by-side runs use this so a quota or cache from one instance can never
    influence another. Without it, two servers on the same machine share the daily quota.
    """
    global AUDIT, USAGE, CACHE
    d = Path(directory)
    d.mkdir(parents=True, exist_ok=True)
    AUDIT = d / "audit.log"
    USAGE = d / "usage.json"
    CACHE = cache.CacheStore(d / "cache.db")


def _scope(principal: dict, tool: str | None) -> str:
    """A personal answer is scoped to its user and can never be served to another one."""
    personal = principal["personal_data"] or bool(policy.TOOLS.get(tool or "", {}).get("personal"))
    return f"user:{principal['user_id']}" if personal else "workspace:default"


def _usage_bucket(uid: str) -> dict:
    today = date.today().isoformat()
    if USAGE.exists():
        data = json.loads(USAGE.read_text(encoding="utf-8"))
    else:
        data = {}
    b = data.get(today, {}).setdefault(uid, {"requests": 0, "tokens": 0})
    return {"file": data, "today": today, "bucket": b}


def _charge(uid: str, tokens: int, limit_req: int, limit_tok: int) -> None:
    with _LOCK:
        u = _usage_bucket(uid)
        u["bucket"]["requests"] += 1
        u["bucket"]["tokens"] += tokens
        u["file"].setdefault(u["today"], {})[uid] = u["bucket"]
        USAGE.write_text(json.dumps(u["file"], indent=2), encoding="utf-8")


def _over_quota(uid: str, limit_req: int, limit_tok: int) -> bool:
    with _LOCK:
        b = _usage_bucket(uid)["bucket"]
        return b["requests"] >= limit_req or b["tokens"] >= limit_tok


def _audit(record: dict) -> None:
    record["ts"] = time.strftime("%Y-%m-%dT%H:%M:%S%z")
    with _LOCK:
        with AUDIT.open("a", encoding="utf-8") as fh:
            fh.write(json.dumps(record, ensure_ascii=False) + "\n")


def _pick_tool(text: str) -> tuple[str, dict] | None:
    low = cache.fold(text)
    match = PATH_RE.search(text)
    if match and any(k in low for k in OCR_HINT):
        return "ocr_extract", {"path": match.group(0)}
    for keys, name, args in INTENT:
        if any(k in low for k in keys):
            return name, args
    return None


def _render(text: str, tool: str, result: dict, principal: dict) -> str:
    """Compose the answer. The model is optional polish, never the source of numbers."""
    facts = json.dumps(result, ensure_ascii=False)
    if tool == "ocr_extract":
        if result.get("error"):
            base = f"Dịch vụ OCR trả lỗi: {result['error']}"
        else:
            head = (result.get("text") or "").strip().splitlines()[:3]
            base = (f"{result.get('chars', 0)} ký tự, {result.get('pages', 1)} trang, "
                    f"engine {result.get('engine')}. " + " / ".join(head))
    elif tool == "salary_self":
        if not result.get("found"):
            base = "Không tìm thấy bản ghi lương của bạn."
        else:
            amount = f"{result['amount']:,}".replace(",", ".")
            base = f"Lương tháng này của bạn là {amount} {result['currency']}."
    elif tool == "docs_search":
        base = "Tài liệu liên quan: " + (", ".join(h["id"] for h in result["hits"]) or "không có")
    else:
        base = "Trạng thái: " + ", ".join(f"{k}={v}" for k, v in result["services"].items())

    polished, usage = model.complete(
        "You are a terse assistant. Keep every number exactly as given. "
        "Answer in the user's language. Never invent figures.",
        f"Facts: {facts}\nQuestion: {text}",
    )
    return (polished or base), usage


def handle(payload: dict, rules: dict, consent: dict | None = None) -> tuple[int, dict]:
    uid = str(payload.get("platform_user_id", ""))
    chat = payload.get("chat_type", "")
    text = (payload.get("text") or "").strip()
    try:
        principal = policy.resolve(rules, uid, chat)
    except policy.Denied as d:
        _audit({"user": uid, "chat": chat, "decision": "denied", "code": d.code})
        return 403, {"denied": {"code": d.code, "reason": d.message}}

    limit_req = int(rules["quota"]["requests_per_day"])
    limit_tok = int(rules["quota"]["tokens_per_day"])
    if _over_quota(principal["user_id"], limit_req, limit_tok):
        _audit({"user": uid, "chat": chat, "decision": "quota_exceeded"})
        return 429, {"denied": {"code": "quota_exceeded", "reason": "daily quota reached"}}

    consent = consent if consent is not None else policy.load_consent(
        ROOT.parent.parent / "config" / "consent.json")
    visible = [t for t in policy.visible_tools(rules, principal) if policy.consent_allows(principal, t, consent)]
    routable = [t for t in policy.routable_tools(rules, principal) if policy.consent_allows(principal, t, consent)]
    picked = _pick_tool(text)
    tool_for_ttl = picked[0] if picked else None
    ttl = TTL.get(tool_for_ttl or "", 0)
    scope = _scope(principal, tool_for_ttl)
    key = cache.exact_key(text, visible, POLICY_VERSION, principal["chat_type"], scope)
    if ttl > 0:
        for hit in (CACHE.get_exact(key), CACHE.get_semantic(text, scope)):
            if hit:
                # A cache hit still costs a request slot but zero tokens: otherwise a
                # cheap-to-serve caller could bypass the daily request quota entirely.
                _charge(principal["user_id"], 0, limit_req, limit_tok)
                _audit({"user": uid, "chat": chat, "decision": f"cache_{hit['layer']}",
                        "scope": scope, "tool": tool_for_ttl})
                return 200, {"delivered": "chat", "chat_reply": hit["answer"], "dm": None,
                             "tools_visible": visible,
                             "cache": {"layer": hit["layer"], "saved_tokens": hit["saved_tokens"],
                                       "similarity": hit.get("similarity")},
                             "usage": {"prompt_tokens": 0, "completion_tokens": 0, "cache": hit["layer"]}}

    # If a provider is configured, the model picks the tool — from the visible list only.
    # This is the difference between "filter before the model" and "deny after the call":
    # a tool that is not in `visible` is not in the request at all.
    offered = [tools.schema(t) for t in visible]
    if model.enabled():
        turn = model.complete_with_tools(SYSTEM, text, offered)
        spent = turn["usage"].get("prompt_tokens", 0) + turn["usage"].get("completion_tokens", 0)
        _charge(principal["user_id"], spent, limit_req, limit_tok)
        if turn["tool_calls"]:
            blocked = [c for c in turn["tool_calls"] if c["name"] not in routable]
            if blocked:
                _audit({"user": uid, "chat": chat, "decision": "tool_not_allowed",
                        "tool": blocked[0]["name"], "offered": visible})
                return 200, {"delivered": "chat", "dm": None,
                             "chat_reply": "Mình không có quyền làm việc này.",
                             "denied_tool": blocked[0]["name"],
                             "tools_visible": visible, "tools_offered": len(offered),
                             "usage": turn["usage"]}
            picked = (turn["tool_calls"][0]["name"], turn["tool_calls"][0]["arguments"])
        elif turn["content"] and policy.free_chat_enabled(rules, principal):
            _audit({"user": uid, "chat": chat, "decision": "free_chat", "tools_visible": visible})
            return 200, {"delivered": "chat", "chat_reply": turn["content"], "dm": None,
                         "tools_visible": visible, "tools_offered": len(offered),
                         "usage": turn["usage"]}

    if picked is None or picked[0] not in routable:
        if model.enabled() and policy.free_chat_enabled(rules, principal):
            reply, usage = model.complete(
                "You are a terse assistant. Answer in the user's language.", text)
            decision = "free_chat"
        else:
            reply, usage = "Mình chưa có công cụ cho việc này.", {
                "prompt_tokens": 0, "completion_tokens": 0, "mock": True}
            decision = "no_tool"
        _charge(principal["user_id"], usage.get("prompt_tokens", 0) + usage.get("completion_tokens", 0), limit_req, limit_tok)
        _audit({"user": uid, "chat": chat, "decision": decision, "tools_visible": visible})
        return 200, {"delivered": "chat", "chat_reply": reply, "dm": None,
                     "tools_visible": visible, "usage": usage}

    tool, args = picked
    try:
        mode = policy.decide(rules, principal, tool)
    except policy.Denied as d:
        _audit({"user": uid, "chat": chat, "decision": "denied", "code": d.code, "tool": tool})
        return 403, {"denied": {"code": d.code, "reason": d.message}}

    try:
        result = tool_call(tool, principal, args)
    except ToolError as exc:
        _audit({"user": uid, "chat": chat, "tool": tool, "decision": "tool_error", "error": str(exc)})
        return 500, {"denied": {"code": "tool_error", "reason": str(exc)}}

    if result.get("error") in ("ocr_unavailable", "ocr_engine_missing"):
        _audit({"user": uid, "chat": chat, "tool": tool, "decision": "tool_unavailable",
                "code": result["error"]})
        # One code for the caller, the specific cause in the reason: two implementations cannot
        # agree on a contract they spell differently.
        return 503, {"denied": {"code": "service_unavailable",
                                "reason": f"{result['error']}: {result.get('detail', '')}".strip()}}

    answer, usage = _render(text, tool, result, principal)
    _charge(principal["user_id"], usage.get("prompt_tokens", 0) + usage.get("completion_tokens", 0), limit_req, limit_tok)

    if ttl > 0:
        CACHE.put_exact(key, answer, visible, int(usage.get("prompt_tokens", 0)) + int(usage.get("completion_tokens", 0)), ttl, scope)
        CACHE.put_semantic(text, answer, int(usage.get("prompt_tokens", 0)) + int(usage.get("completion_tokens", 0)), ttl, scope)

    if mode == "call_and_deliver_privately":
        _audit({"user": uid, "chat": chat, "tool": tool, "decision": "private_delivery"})
        # `dm` is an object, not a bare string: text to deliver + the reason it cannot
        # be shown in this chat. The adapter must read `dm.text`.
        return 200, {"delivered": "dm", "chat_reply": "Mình gửi riêng cho bạn nhé.",
                     "dm": {"text": answer, "reason": "personal_data_in_shared_chat"},
                     "tools_visible": visible, "usage": usage}

    _audit({"user": uid, "chat": chat, "tool": tool, "decision": "answered"})
    return 200, {"delivered": "chat", "chat_reply": answer, "dm": None,
                 "tools_visible": visible, "usage": usage}


class Handler(BaseHTTPRequestHandler):
    rules: dict = {}

    def _json(self, code: int, body: dict) -> None:
        raw = json.dumps(body, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self) -> None:
        url = urlparse(self.path)
        if url.path == "/health":
            return self._json(200, {"ok": True})
        if url.path == "/stats/cache":
            return self._json(200, {**CACHE.stats(),
                                    "l3_prefix": cache.prefix_fingerprint(
                                        sorted(policy.TOOLS.keys()), POLICY_VERSION)})
        if url.path == "/stats":
            uid = parse_qs(url.query).get("platform_user_id", [""])[0]
            b = _usage_bucket(uid)["bucket"]
            return self._json(200, {"user": uid, "today": {**b}})
        self._json(404, {"error": "not found"})

    def do_POST(self) -> None:
        if urlparse(self.path).path != "/v1/respond":
            return self._json(404, {"error": "not found"})
        length = int(self.headers.get("Content-Length", "0"))
        try:
            payload = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            return self._json(400, {"error": "bad json"})
        code, body = handle(payload, self.rules, getattr(self, "consent", None))
        self._json(code, body)

    def log_message(self, fmt: str, *args) -> None:  # keep stdout clean
        return


def make_server(policy_path: str | None = None, port: int = 8088,
                state_dir: str | Path | None = None) -> ThreadingHTTPServer:
    path = policy_path or os.environ.get("POLICY_PATH", str(ROOT / "config" / "policy.json"))
    # STATE_DIR was documented but never read: two servers silently shared one cache.db.
    if state_dir is None and os.environ.get("STATE_DIR"):
        state_dir = os.environ["STATE_DIR"]
    if state_dir is not None:
        install_state(state_dir)
    rules = policy.load_rules(path)
    local = policy.load_local(ROOT.parent.parent / "config" / "local.json")
    consent_path = ROOT.parent.parent / "config" / "consent.json"
    consent = policy.load_consent(consent_path)
    rules, consent = policy.apply_local(rules, consent, local)
    tools.load_salary_overrides(local.get("salary"))
    Handler.rules = rules
    Handler.consent = consent
    srv = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    # Default backlog is 5; 32 concurrent clients is more than that. The connection resets I
    # first blamed on this were actually the shared sqlite connection (see gateway/cache.py),
    # but a listen queue of 5 is still the wrong default for a gateway.
    srv.request_queue_size = 128
    return srv


if __name__ == "__main__":
    srv = make_server(port=int(os.environ.get("PORT", "8088")))
    print(f"gateway on http://127.0.0.1:{srv.server_address[1]}  (policy: {os.environ.get('POLICY_PATH', 'config/policy.json')})")
    srv.serve_forever()
