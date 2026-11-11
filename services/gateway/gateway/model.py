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
