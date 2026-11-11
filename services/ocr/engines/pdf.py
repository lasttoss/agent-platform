"""PDF text extraction. Prefers poppler's pdftotext, falls back to pypdf.

Returns text plus a `scanned` flag: a PDF whose pages carry almost no text is a
scan, and a scan needs an OCR engine (reported, never silently returned empty).
"""
from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

MIN_CHARS_PER_PAGE = 40


def _pdftotext(path: Path, timeout: int) -> tuple[str, str]:
    exe = shutil.which("pdftotext")
    if not exe:
        raise FileNotFoundError("pdftotext")
    out = subprocess.run(
        [exe, "-layout", "-enc", "UTF-8", str(path), "-"],
        capture_output=True, timeout=timeout, check=False,
    )
    if out.returncode != 0:
        raise RuntimeError(out.stderr.decode("utf-8", "replace")[:300])
    return out.stdout.decode("utf-8", "replace"), "pdftotext"


def _pypdf(path: Path) -> tuple[str, str]:
    from pypdf import PdfReader

    reader = PdfReader(str(path))
    pages = [(p.extract_text() or "") for p in reader.pages]
    return "\n\f\n".join(pages), "pypdf"


def extract_pdf(path: Path, timeout: int = 20) -> dict:
    engine = ""
    try:
        text, engine = _pdftotext(path, timeout)
    except (FileNotFoundError, RuntimeError):
        text, engine = _pypdf(path)

    # pdftotext terminates every page with \f, including the last one; adding 1
    # blindly reports one page too many.
    pages = text.count("\f") or 1
    if text and not text.endswith("\f"):
        pages = text.count("\f") + 1
    chars = len(text.strip())
    scanned = chars < MIN_CHARS_PER_PAGE * pages
    return {
        "text": text.strip(),
        "chars": chars,
        "pages": pages,
        "engine": engine,
        "scanned": scanned,
        "warning": "no text layer: needs an OCR engine" if scanned else None,
    }
