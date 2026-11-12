"""MCP boundary for the OCR service.

The gateway ships no extraction code: it calls `tools/call` and gets one JSON document back.
This adapter stays a thin translation of `run()` on purpose — who may call it is the gateway's
question, can this file be read is the engine's question, and neither belongs in the other.
"""
from __future__ import annotations

from typing import Any, Callable

TOOL: dict[str, Any] = {
    "name": "ocr_extract",
    "description": "Extract text from a PDF or image inside the service root.",
    "inputSchema": {
        "type": "object",
        "properties": {
            "path": {"type": "string", "description": "Path relative to the service root."},
            "lang": {"type": "string", "description": "Tesseract language, e.g. eng."},
        },
        "required": ["path"],
    },
}


def handle(body: dict[str, Any], run: Callable[[dict], tuple[int, dict]]) -> dict[str, Any]:
    method = body.get("method")
    rid = body.get("id")

    def ok(result: Any) -> dict[str, Any]:
        return {"jsonrpc": "2.0", "id": rid, "result": result}

    if method == "initialize":
        return ok({
            "protocolVersion": "2024-11-05",
            "serverInfo": {"name": "ocr", "version": "1.0.0"},
            "capabilities": {"tools": {}},
        })
    if method in ("notifications/initialized", "ping"):
        return ok({})
    if method == "tools/list":
        return ok({"tools": [TOOL]})
    if method == "tools/call":
        params = body.get("params") or {}
        if params.get("name") != TOOL["name"]:
            return ok({"isError": True,
                       "content": [{"type": "text", "text": f"unknown tool: {params.get('name')}"}]})
        code, payload = run(params.get("arguments") or {})
        import json
        return ok({"isError": code >= 400,
                   "content": [{"type": "text",
                                "text": json.dumps(payload, ensure_ascii=False)}]})
    return {"jsonrpc": "2.0", "id": rid,
            "error": {"code": -32601, "message": f"method not found: {method}"}}
