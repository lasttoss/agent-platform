"""Run the Python and the Go gateway side by side and diff their answers.

Two implementations are only worth having if they agree. This script is the referee: same
policy file, same requests, compare the decisions — not the prose formatting, the decisions.
Exit code is 1 on any mismatch, so CI can run it.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
PY_DIR = ROOT / "services" / "gateway"
GO_DIR = ROOT / "services" / "gateway-go"
PY_PORT, GO_PORT = 8171, 8172

CASES = [
    ("1001", "dm", "lương tháng này của tôi"),
    ("1001", "dm", "luong thang nay cua toi"),
    ("2002", "dm", "lương tháng này của tôi"),
    ("1001", "group_internal", "lương tháng này của tôi"),
    ("1001", "group_public", "lương tháng này của tôi"),
    ("9999", "dm", "lương tháng này của tôi"),
    ("1001", "dm", "tài liệu runbook"),
    ("1001", "dm", "tài liệu runbook"),      # second call: cache
    ("1001", "dm", "thời tiết hôm nay thế nào"),
    ("2002", "dm", "trạng thái"),
]


def post(port: int, payload: dict) -> tuple[int, dict]:
    req = urllib.request.Request(f"http://127.0.0.1:{port}/v1/respond",
                                 data=json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode())


def port_free(port: int) -> bool:
    """A parity run that silently compares against a leftover process proves nothing:
    an earlier `go run` child kept the port and the new build never started."""
    import socket
    with socket.socket() as sock:
        sock.settimeout(0.3)
        return sock.connect_ex(("127.0.0.1", port)) != 0


def wait(port: int, timeout: float = 25.0) -> None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/health", timeout=2):
                return
        except Exception:
            time.sleep(0.2)
    raise SystemExit(f"port {port} never became healthy")


def main() -> int:
    # Fresh state for both sides. Python persists its cache in SQLite and Go keeps it in memory,
    # so a warm Python cache from a previous run would show up as a "difference" that is really
    # a leftover. A parity run must start cold or it measures the last run.
    state = Path(tempfile.mkdtemp())
    env = dict(os.environ, PORT=str(PY_PORT), STATE_DIR=str(state / "python"))
    for port in (PY_PORT, GO_PORT):
        if not port_free(port):
            raise SystemExit(f"port {port} already in use — refusing to compare against a "
                             f"process this script did not start")

    # Build first, then run the binary: terminating `go run` leaves its child alive,
    # which is exactly how a stale server can masquerade as a fresh one.
    binary = Path(tempfile.mkdtemp()) / "gateway-go"
    subprocess.run(["go", "build", "-o", str(binary), "."], cwd=GO_DIR, check=True)

    py = subprocess.Popen([sys.executable, "-m", "gateway.server"], cwd=PY_DIR, env=env,
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    go = subprocess.Popen([str(binary), "-addr", f"127.0.0.1:{GO_PORT}",
                           "-audit", str(state / "go-audit.log")],
                          cwd=GO_DIR, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    try:
        wait(PY_PORT)
        wait(GO_PORT)
        mismatches = 0
        for user, chat, text in CASES:
            payload = {"platform_user_id": user, "chat_type": chat, "text": text}
            py_code, py_body = post(PY_PORT, payload)
            go_code, go_body = post(GO_PORT, payload)
            for kind in (py_body, go_body):
                if isinstance(kind.get("dm"), dict):
                    kind["dm"] = kind["dm"].get("text")
                for drop in ("usage", "tools_offered"):
                    kind.pop(drop, None)
                # JSON null vs absent: the same statement about the same field
                for key in [k for k, v in kind.items() if v is None]:
                    kind.pop(key)
                if isinstance(kind.get("cache"), dict):
                    kind["cache"] = {k: v for k, v in kind["cache"].items() if v is not None}
            same = py_code == go_code and py_body == go_body
            mismatches += 0 if same else 1
            mark = "same" if same else "DIFF"
            print(f"  [{mark}] {user:>4} {chat:<14} {text[:34]:<36} "
                  f"py={py_code} go={go_code}")
            if not same:
                print(f"        python: {json.dumps(py_body, ensure_ascii=False)}")
                print(f"        go    : {json.dumps(go_body, ensure_ascii=False)}")
        print(f"\n{len(CASES) - mismatches}/{len(CASES)} cases identical")
        return 0 if mismatches == 0 else 1
    finally:
        for proc in (py, go):
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()


if __name__ == "__main__":
    sys.exit(main())
