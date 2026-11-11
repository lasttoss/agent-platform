"""Image OCR. Uses tesseract when installed; otherwise reports the gap honestly
instead of pretending an empty string is a result."""
from __future__ import annotations

import shutil
import subprocess
from pathlib import Path


def engine_report() -> dict:
    tess = shutil.which("tesseract")
    return {
        "pdf": {"pdftotext": bool(shutil.which("pdftotext")), "pypdf": _has_pypdf()},
        "image": {"tesseract": bool(tess)},
        "image_ocr_available": bool(tess),
    }


def _has_pypdf() -> bool:
    try:
        import pypdf  # noqa: F401
        return True
    except Exception:
        return False


def extract_image(path: Path, lang: str = "eng", timeout: int = 20) -> dict:
    exe = shutil.which("tesseract")
    if not exe:
        return {
            "text": "",
            "chars": 0,
            "engine": None,
            "scanned": True,
            "error": "ocr_engine_missing",
            "detail": "tesseract is not installed on this host; install tesseract-ocr to enable image OCR",
        }
    out = subprocess.run(
        [exe, str(path), "stdout", "-l", lang],
        capture_output=True, timeout=timeout, check=False,
    )
    text = out.stdout.decode("utf-8", "replace").strip()
    if out.returncode != 0:
        return {"text": "", "chars": 0, "engine": "tesseract", "scanned": True,
                "error": "ocr_failed", "detail": out.stderr.decode("utf-8", "replace")[:300]}
    return {"text": text, "chars": len(text), "engine": f"tesseract:{lang}", "scanned": False}
