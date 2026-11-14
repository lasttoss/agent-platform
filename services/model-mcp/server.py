"""The model as a service, behind MCP.

The gateway owns policy; this service answers "which tool fits this sentence". It has no
access to any tool, no caller identity it can trust, and no ability to act — it returns a
name and arguments, and the gateway decides whether that call is allowed. Without
MODEL_BASE_URL it returns an empty turn instead of pretending to answer: a model service that
invents a reply when unconfigured is worse than one that says nothing.
"""
from __future__ import annotations

import json
import os
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TOOL = {
    "name": "chat_complete",
    "description": "Return a completion, optionally choosing one of the offered tools.",
    "inputSchema": {
        "type": "object",
        "properties": {
            "system": {"type": "string"},
            "user": {"type": "string"},
            "tools": {"type": "array", "items": {"type": "object"}},
        },
        "required": ["user"],
    },
}

SYSTEM_FALLBACK = ("You are a gateway assistant. Use a tool whenever one fits. "
                   "Never invent numbers, names or status.")


def complete(args: dict) -> dict:
    """One turn. `usage` is 0 when mocked, so a cached answer and a free answer look identical."""
    user = str(args.get("user", ""))[:4000]
    system = str(args.get("system", "") or SYSTEM_FALLBACK)[:4000]
    tools = args.get("tools") or []

    base = os.environ.get("MODEL_BASE_URL", "").rstrip("/")
    if not base:
        forced = os.environ.get("FAKE_TOOL_CALL", "")
        if forced:
            return {"content": "", "mock": True,
                    "tool_calls": [{"name": forced, "arguments": {}}],
                    "usage": {"prompt_tokens": 0, "completion_tokens": 0}}
        return {"content": "", "tool_calls": [], "mock": True,
                "usage": {"prompt_tokens": 0, "completion_tokens": 0}}

    # A caller may name the model (the gateway's OpenAI-shaped endpoint checks its whitelist
    # first); otherwise the deployment's configured model is the only one on offer.
    body = {"model": str(args.get("model") or os.environ.get("MODEL_NAME", "gpt-4o-mini")),
            "temperature": 0.2,
            "messages": [{"role": "system", "content": system},
                         {"role": "user", "content": user}]}
    if tools:
        body["tools"] = tools
    req = urllib.request.Request(f"{base}/chat/completions",
                                 data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json",
                                          # A default urllib User-Agent is refused by at least
                                          # one provider's edge (403, measured), so this service
                                          # names itself. A client that hides what it is gets
                                          # blocked by something that is trying to be helpful.
                                          "User-Agent": "agent-platform-model/1.0",
                                          "Authorization": f"Bearer {os.environ.get('MODEL_API_KEY', '')}"})
    with urllib.request.urlopen(req, timeout=60) as resp:
        parsed = json.loads(resp.read().decode())

    choice = (parsed.get("choices") or [{}])[0].get("message", {})
    calls = []
    for call in choice.get("tool_calls") or []:
        fn = call.get("function", {})
        raw = fn.get("arguments") or "{}"
        try:
            parsed_args = json.loads(raw) if isinstance(raw, str) else dict(raw)
        except json.JSONDecodeError:
            parsed_args = {}
        # The gateway needs objects, not JSON-encoded strings: parse once, here, so neither
        # side has to guess the other's idea of "arguments".
        calls.append({"name": fn.get("name", ""), "arguments": parsed_args})
    usage = parsed.get("usage") or {}
    return {"content": choice.get("content") or "", "tool_calls": calls, "mock": False,
            "usage": {"prompt_tokens": usage.get("prompt_tokens", 0),
                      "completion_tokens": usage.get("completion_tokens", 0)}}


def handle(body: dict) -> dict:
    method, rid = body.get("method"), body.get("id")

    def ok(result):
        return {"jsonrpc": "2.0", "id": rid, "result": result}

    if method == "initialize":
        return ok({"protocolVersion": "2024-11-05",
                   "serverInfo": {"name": "model", "version": "1.0.0"},
                   "capabilities": {"tools": {}}})
    if method in ("notifications/initialized", "ping"):
        return ok({})
    if method == "tools/list":
        return ok({"tools": [TOOL]})
    if method == "tools/call":
        params = body.get("params") or {}
        if params.get("name") != TOOL["name"]:
            return ok({"isError": True, "content": [{"type": "text", "text": "unknown tool"}]})
        try:
            result = complete(params.get("arguments") or {})
        except Exception as exc:  # an upstream failure is reported, never swallowed
            return ok({"isError": True,
                       "content": [{"type": "text", "text": f"model_unavailable: {exc}"}]})
        return ok({"isError": False,
                   "content": [{"type": "text", "text": json.dumps(result, ensure_ascii=False)}]})
    return {"jsonrpc": "2.0", "id": rid, "error": {"code": -32601, "message": "method not found"}}


class Handler(BaseHTTPRequestHandler):
    def do_POST(self) -> None:
        if self.path.rstrip("/") != "/mcp":
            return self._json(404, {"error": "not_found"})
        size = int(self.headers.get("Content-Length", "0"))
        try:
            body = json.loads(self.rfile.read(size) or b"{}")
        except json.JSONDecodeError:
            return self._json(400, {"error": "bad_json"})
        self._json(200, handle(body))

    def do_GET(self) -> None:
        if self.path.rstrip("/") == "/health":
            return self._json(200, {"ok": True, "mock": not os.environ.get("MODEL_BASE_URL")})
        self._json(404, {"error": "not_found"})

    def _json(self, code: int, body: dict) -> None:
        raw = json.dumps(body, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, fmt: str, *args) -> None:
        return


def make_server(port: int = 8097) -> ThreadingHTTPServer:
    # Loopback by default; a container must opt in with HOST=0.0.0.0.
    return ThreadingHTTPServer((os.environ.get("HOST", "127.0.0.1"), port), Handler)


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "8097"))
    print(f"model-mcp on :{port}/mcp")
    make_server(port).serve_forever()
