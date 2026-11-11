"""Model adapter. Optional: the gateway works without any provider.

If MODEL_BASE_URL is set, it is called as an OpenAI-compatible chat endpoint.
Otherwise the gateway runs in mock mode so policy tests never need a key.
"""
from __future__ import annotations

import json
import os
import urllib.error
import urllib.request


def enabled() -> bool:
    return bool(os.environ.get("MODEL_BASE_URL", "").rstrip("/"))


def complete(system: str, user: str) -> tuple[str, dict]:
    base = os.environ.get("MODEL_BASE_URL", "").rstrip("/")
    key = os.environ.get("MODEL_API_KEY", "")
    name = os.environ.get("MODEL_NAME", "gpt-4o-mini")
    if not base:
        return ("", {"prompt_tokens": 0, "completion_tokens": 0, "mock": True})

    body = json.dumps({
        "model": name,
        "messages": [{"role": "system", "content": system}, {"role": "user", "content": user}],
        "temperature": 0.2,
    }).encode()
    req = urllib.request.Request(
        f"{base}/chat/completions",
        data=body,
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {key}"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            data = json.loads(resp.read().decode())
    except (urllib.error.URLError, TimeoutError) as exc:
        return ("", {"prompt_tokens": 0, "completion_tokens": 0, "error": str(exc)})
    text = data["choices"][0]["message"]["content"]
    usage = data.get("usage", {})
    return (text, {"prompt_tokens": usage.get("prompt_tokens", 0),
                   "completion_tokens": usage.get("completion_tokens", 0), "mock": False})


def complete_with_tools(system: str, user: str, schemas: list[dict]) -> dict:
    """One turn with a tool list attached.

    Returns {"content", "tool_calls": [{"name", "arguments"}], "usage", "mock"}.
    The model is never trusted to decide whether it may run a tool: it only says which one
    it wants, and the gateway decides. Sending fewer tools is the cheapest way to remove a
    capability — a tool that is not in `tools` cannot be called at all.
    """
    if not enabled():
        return {"content": "", "tool_calls": [], "mock": True,
                "usage": {"prompt_tokens": 0, "completion_tokens": 0}}
    base = os.environ["MODEL_BASE_URL"].rstrip("/")
    body = json.dumps({
        "model": os.environ.get("MODEL_NAME", "gpt-4o-mini"),
        "messages": [{"role": "system", "content": system}, {"role": "user", "content": user}],
        "tools": schemas,
        "temperature": 0.2,
    }).encode()
    req = urllib.request.Request(
        f"{base}/chat/completions", data=body,
        headers={"Content-Type": "application/json",
                 "Authorization": f"Bearer {os.environ.get('MODEL_API_KEY', '')}"},
        method="POST")
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            data = json.loads(resp.read().decode())
    except (urllib.error.URLError, TimeoutError) as exc:
        return {"content": "", "tool_calls": [], "mock": False, "error": str(exc),
                "usage": {"prompt_tokens": 0, "completion_tokens": 0}}

    message = data["choices"][0]["message"]
    calls = []
    for call in message.get("tool_calls") or []:
        fn = call.get("function") or {}
        try:
            args = json.loads(fn.get("arguments") or "{}")
        except json.JSONDecodeError:
            args = {}
        calls.append({"name": fn.get("name", ""), "arguments": args})
    usage = data.get("usage", {})
    return {"content": message.get("content") or "", "tool_calls": calls, "mock": False,
            "usage": {"prompt_tokens": usage.get("prompt_tokens", 0),
                      "completion_tokens": usage.get("completion_tokens", 0)}}
