"""OCR tool service: one contract, hard guards, no policy.

Contract (see README): POST /v1/ocr {"path": "samples/x.pdf", "lang": "eng"}
Answers: {text, chars, pages, engine, scanned, warning|error, elapsed_ms}

This service does not know who is calling and must never decide who is allowed.
The gateway in front of it owns identity and authority; this owns the work.
"""
from __future__ import annotations

import json
import os
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from engines import engine_report, extract_image, extract_pdf

ROOT = Path(os.environ.get("TOOL_ROOT", Path(__file__).resolve().parent)).resolve()
MAX_BYTES = int(os.environ.get("MAX_BYTES", str(15 * 1024 * 1024)))
TIMEOUT_S = int(os.environ.get("OCR_TIMEOUT", "20"))
ALLOWED = {".pdf", ".png", ".jpg", ".jpeg", ".tif", ".tiff", ".bmp", ".webp"}


def _resolve(raw: str) -> Path:
    """Path guard: inside ROOT only, no symlink escape, real file, allowed suffix."""
    if not raw:
        raise ValueError("path_required")
    p = (ROOT / urllib.parse.unquote(raw)).resolve() if not os.path.isabs(raw) else Path(raw).resolve()
    if ROOT not in p.parents and p != ROOT:
        raise PermissionError("path_outside_root")
    if p.suffix.lower() not in ALLOWED:
        raise ValueError(f"suffix_not_allowed:{p.suffix}")
    if not p.is_file():
        raise FileNotFoundError("not_a_file")
    size = p.stat().st_size
    if size > MAX_BYTES:
        raise ValueError(f"file_too_large:{size}")
    if size == 0:
        raise ValueError("empty_file")
    return p


def run(payload: dict) -> tuple[int, dict]:
    started = time.monotonic()
    try:
        path = _resolve(str(payload.get("path", "")))
    except PermissionError as exc:
        return 403, {"error": str(exc)}
    except ValueError as exc:
        return 400, {"error": str(exc)}
    except FileNotFoundError as exc:
        return 404, {"error": str(exc)}

    lang = str(payload.get("lang", "eng"))[:12]
    try:
        if path.suffix.lower() == ".pdf":
            result = extract_pdf(path, timeout=TIMEOUT_S)
        else:
            result = extract_image(path, lang=lang, timeout=TIMEOUT_S)
    except Exception as exc:  # a broken pipeline is a 500, never a silent empty answer
        return 500, {"error": "extraction_failed", "detail": str(exc)[:300]}

    result["file"] = str(path.relative_to(ROOT)) if ROOT in path.parents else path.name
    result["elapsed_ms"] = round((time.monotonic() - started) * 1000, 1)
    return (200 if not result.get("error") else 503), result


class Handler(BaseHTTPRequestHandler):
    def _json(self, code: int, body: dict) -> None:
        raw = json.dumps(body, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self) -> None:
        if urllib.parse.urlparse(self.path).path == "/health":
            rep = engine_report()
            return self._json(200, {"ok": True, "engines": rep, "root": str(ROOT),
                                    "max_bytes": MAX_BYTES, "timeout_s": TIMEOUT_S})
        self._json(404, {"error": "not_found"})

    def do_POST(self) -> None:
        if urllib.parse.urlparse(self.path).path != "/v1/ocr":
            return self._json(404, {"error": "not_found"})
        n = int(self.headers.get("Content-Length", "0"))
        if n > MAX_BYTES:
            return self._json(413, {"error": "payload_too_large"})
        try:
            payload = json.loads(self.rfile.read(n) or b"{}")
        except json.JSONDecodeError:
            return self._json(400, {"error": "bad_json"})
        code, body = run(payload)
        self._json(code, body)

    def log_message(self, fmt: str, *args) -> None:
        return


def make_server(port: int = 8099, root: str | None = None) -> ThreadingHTTPServer:
    global ROOT
    if root:
        ROOT = Path(root).resolve()
    return ThreadingHTTPServer(("127.0.0.1", port), Handler)


if __name__ == "__main__":
    srv = make_server(int(os.environ.get("PORT", "8099")))
    print(f"ocr tool service on http://127.0.0.1:{srv.server_address[1]}  root={ROOT}")
    srv.serve_forever()
