"""A tool result belongs in memory as a pointer, not as a body.

Measured on this repo's own traffic shape, the store does not grow because people remember things.
It grows because one tool answered with 200 KB of file content and every layer above dutifully
copied it: into the request, into the log, into the memory row, and into the next prompt.

So the rule here is one sentence: **the memory row holds where the bytes are, plus a hash that
says whether they are still the same bytes.** A reference is a few hundred bytes and never grows
with the result. Reading it back is a deliberate act with a byte ceiling, because "just inline the
whole thing" is how the growth comes back.

`sha256` is the part that earns its keep: file paths get reused, so a path alone will happily point
at tomorrow's content. The hash makes a stale reference detectable instead of silently wrong.
"""
from __future__ import annotations

import hashlib
import json
import os
import time
from pathlib import Path

CHUNK = 1 << 20  # hash in 1 MB chunks: a 2 GB log file should not be read into memory to be named


def _digest(path: Path) -> tuple[str, int]:
    h = hashlib.sha256()
    size = 0
    with path.open("rb") as fh:
        while chunk := fh.read(CHUNK):
            h.update(chunk)
            size += len(chunk)
    return h.hexdigest(), size


def capture(path: str | os.PathLike[str]) -> dict:
    """Describe a file that already exists. Nothing is copied."""
    p = Path(path)
    if not p.is_file():
        raise FileNotFoundError(f"{p} is not a file")
    digest, size = _digest(p)
    return {
        "kind": "file",
        "path": str(p.resolve()),
        "sha256": digest,
        "bytes": size,
        "mtime": int(p.stat().st_mtime),
    }


def spool(data: bytes | str, name: str, directory: str | os.PathLike[str]) -> dict:
    """Write a tool result to a spool directory and return its reference.

    The name is prefixed with the digest, so the same content written twice lands on the same
    file instead of filling the disk with copies that differ only by timestamp.
    """
    payload = data.encode("utf-8") if isinstance(data, str) else data
    digest = hashlib.sha256(payload).hexdigest()
    root = Path(directory)
    root.mkdir(parents=True, exist_ok=True)
    target = root / f"{digest[:16]}-{Path(name).name}"
    if not target.exists():
        target.write_bytes(payload)
    return {
        "kind": "spool",
        "path": str(target.resolve()),
        "sha256": digest,
        "bytes": len(payload),
        "mtime": int(time.time()),
    }


def dumps(ref: dict) -> str:
    """The JSON that goes in a memory body. Sorted keys so two equal refs hash equal."""
    return json.dumps(ref, sort_keys=True, separators=(",", ":"))


def load(body: str) -> dict:
    ref = json.loads(body)
    for field in ("path", "sha256", "bytes"):
        if field not in ref:
            raise ValueError(f"not a reference: missing {field}")
    return ref


def verify(ref: dict) -> bool:
    """True when the bytes at `path` are still the bytes this reference was made from."""
    p = Path(ref["path"])
    if not p.is_file():
        return False
    digest, _ = _digest(p)
    return digest == ref["sha256"]


def _trim_partial_utf8(data: bytes) -> bytes:
    """Drop a character the byte ceiling cut in half.

    A ceiling counted in bytes lands in the middle of a multi-byte character about one time in
    three for Vietnamese text. Decoding that with `errors="replace"` produces a replacement
    character at the end of every truncated read -- which looks like a corrupted result rather
    than a bounded one. Cutting back to the last complete character is the honest answer.
    """
    for cut in range(0, 4):
        if cut >= len(data):
            return b""
        candidate = data[: len(data) - cut]
        try:
            candidate.decode("utf-8")
            return candidate
        except UnicodeDecodeError:
            continue
    return b""


def read(ref_json: str, max_bytes: int = 4096) -> str:
    """Read back at most `max_bytes` from a reference. The ceiling is the point."""
    ref = load(ref_json)
    with Path(ref["path"]).open("rb") as fh:
        return _trim_partial_utf8(fh.read(max_bytes)).decode("utf-8")
