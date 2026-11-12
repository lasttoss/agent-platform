"""MCP boundary for the OCR service.

The gateway ships no extraction code: it calls `tools/call` and gets one JSON document back.
This adapter stays a thin translation of the service's own entry points on purpose — who may
call it is the gateway's question, can this file be read is the engine's question, and neither
belongs in the other.

Two tools, because extraction is slow and a chat turn is not:

- `ocr_extract(path)` submits a job and returns immediately with a job id;
- `ocr_result(job_id)` reads that job.

`ocr_extract` keeps a synchronous mode (`wait_seconds`) so a caller that genuinely wants to block
can still do it — but the default is the queue, because a 40-page PDF holds an in-flight slot for
thirty seconds and four full slots turn every other request into a 503.
"""
from __future__ import annotations

import json
import time
from typing import Any, Callable

EXTRACT: dict[str, Any] = {
    "name": "ocr_extract",
    "description": "Submit a document for text extraction; returns a job id.",
    "inputSchema": {
        "type": "object",
        "properties": {
            "path": {"type": "string", "description": "Path relative to the service root."},
            "lang": {"type": "string", "description": "Tesseract language, e.g. eng."},
            "wait_seconds": {"type": "integer",
                             "description": "Block up to this long instead of returning a job id. 0 = queue only."},
        },
        "required": ["path"],
    },
}

RESULT: dict[str, Any] = {
    "name": "ocr_result",
    "description": "Read the result of a submitted extraction job.",
    "inputSchema": {
        "type": "object",
        "properties": {"job_id": {"type": "string"}},
        "required": ["job_id"],
    },
}

TOOLS = [EXTRACT, RESULT]


def _submit(args: dict[str, Any], run: Callable[[dict], tuple[int, dict]],
            jobs: Any) -> dict[str, Any]:
    code, payload = run(args)  # validates the path through the same guard as the sync route
    if code >= 400:
        return {"isError": True, "code": code, **payload}
    if "error" in payload and not payload.get("chars"):
        return {"isError": True, "code": 503, **payload}
    job = jobs.submit(str(args["path"]), str(args.get("lang", "eng"))[:12])
    return {"job_id": job["id"], "status": job["status"],
            "note": "queued; call ocr_result with this job id"}


def _wait(job_id: str, jobs: Any, seconds: float) -> dict[str, Any]:
    deadline = time.monotonic() + seconds
    while True:
        job = jobs.get(job_id) or {"status": "missing"}
        if job["status"] in ("done", "failed", "expired", "rejected"):
            return job
        if time.monotonic() >= deadline:
            return job
        time.sleep(0.05)


def handle(body: dict[str, Any], run: Callable[[dict], tuple[int, dict]],
           jobs: Any = None, sync_extract: Callable[[dict], tuple[int, dict]] | None = None) -> dict[str, Any]:
    """`jobs` is optional so the adapter is still usable without a queue (a test, a CLI).
    Without it, ocr_extract answers synchronously rather than pretending a job exists."""
    method = body.get("method")
    rid = body.get("id")

    def ok(result: Any) -> dict[str, Any]:
        return {"jsonrpc": "2.0", "id": rid, "result": result}

    def tool_result(payload: dict[str, Any]) -> dict[str, Any]:
        return ok({"isError": bool(payload.get("isError")),
                   "content": [{"type": "text", "text": json.dumps(payload, ensure_ascii=False)}]})

    if method == "initialize":
        return ok({
            "protocolVersion": "2024-11-05",
            "serverInfo": {"name": "ocr", "version": "1.1.0"},
            "capabilities": {"tools": {}},
        })
    if method in ("notifications/initialized", "ping"):
        return ok({})
    if method == "tools/list":
        return ok({"tools": TOOLS})
    if method == "tools/call":
        params = body.get("params") or {}
        name = params.get("name")
        args = params.get("arguments") or {}
        if name == EXTRACT["name"]:
            wait = float(args.get("wait_seconds") or 0)
            if wait <= 0 and jobs is not None:
                return tool_result(_submit(args, run, jobs))
            code, payload = (sync_extract or run)(args)
            return tool_result({**payload, "isError": code >= 400})
        if name == RESULT["name"]:
            if jobs is None:
                return tool_result({"isError": True, "error": "no_job_store"})
            job_id = str(args.get("job_id", "")).strip()
            if not job_id:
                return tool_result({"isError": True, "error": "job_id_required"})
            job = jobs.get(job_id)
            if job is None:
                return tool_result({"isError": True, "error": "job_not_found", "job_id": job_id})
            return tool_result(job)
        return tool_result({"isError": True, "error": f"unknown tool: {name}"})
    return {"jsonrpc": "2.0", "id": rid,
            "error": {"code": -32601, "message": f"method not found: {method}"}}
