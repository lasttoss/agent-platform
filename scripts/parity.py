"""Run the Python and the Go gateway side by side and diff their decisions.

Two implementations are only worth having if they agree. This script is the referee: same
policy file, same requests, compare what the caller experiences — refused or answered, what
text, from cache or not.

One difference is expected and printed every run: the Go gateway lists only tools that a
running MCP service advertises, while the Python reference still carries a built-in registry.
The Go behaviour is the intended one, so the tool lists are reported, not compared.
"""
from __future__ import annotations

import json
import os
import socket
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
PY_PORT, GO_PORT, DEMO_PORT, MODEL_PORT = 8171, 8172, 8181, 8182

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

COMPARED = ("delivered", "chat_reply", "denied", "cache")


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
    """A parity run that silently compares against a leftover process proves nothing."""
    with socket.socket() as sock:
        sock.settimeout(0.3)
        return sock.connect_ex(("127.0.0.1", port)) != 0


def wait(port: int, timeout: float = 25.0, path: str = "/health") -> None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=2):
                return
        except Exception:
            time.sleep(0.2)
    raise SystemExit(f"port {port} never became healthy")


def shape(body: dict) -> dict:
    out = {}
    for key in COMPARED:
        value = body.get(key)
        if isinstance(value, dict):
            value = value.get("code") if key == "denied" else value.get("layer")
        out[key] = value
    return out


def main() -> int:
    for port in (PY_PORT, GO_PORT, DEMO_PORT, MODEL_PORT):
        if not port_free(port):
            raise SystemExit(f"port {port} already in use — refusing to compare against a "
                             f"process this script did not start")

    # Fresh state for both sides: a warm cache from a previous run is a leftover, not a
    # behavioural difference. Build first, then run the binary — terminating `go run` leaves
    # its child alive, which is how a stale server masquerades as a fresh one.
    state = Path(tempfile.mkdtemp())
    demo_bin = state / "mcp-demo"
    gw_bin = state / "gateway-go"
    subprocess.run(["go", "build", "-o", str(demo_bin), "./cmd/mcp-demo"], cwd=GO_DIR, check=True)
    subprocess.run(["go", "build", "-o", str(gw_bin), "."], cwd=GO_DIR, check=True)

    mcp_conf = state / "mcp.json"
    mcp_conf.write_text(json.dumps({"servers": [
        {"name": "demo-tools", "url": f"http://127.0.0.1:{DEMO_PORT}/mcp",
         "tools": ["salary_self", "docs_search", "status"]},
        {"name": "model", "url": f"http://127.0.0.1:{MODEL_PORT}/mcp", "role": "model"},
    ]}))

    env = dict(os.environ, PORT=str(PY_PORT), STATE_DIR=str(state / "python"))
    procs = [
        subprocess.Popen([str(demo_bin), "-addr", f"127.0.0.1:{DEMO_PORT}"],
                         cwd=GO_DIR, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL),
        subprocess.Popen([sys.executable, str(ROOT / "services" / "model-mcp" / "server.py")],
                         env=dict(os.environ, PORT=str(MODEL_PORT)),
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL),
        subprocess.Popen([sys.executable, "-m", "gateway.server"], cwd=PY_DIR, env=env,
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL),
        subprocess.Popen([str(gw_bin), "-addr", f"127.0.0.1:{GO_PORT}",
                          "-mcp", str(mcp_conf),
                          "-audit", str(state / "go-audit.log")],
                         cwd=GO_DIR, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE),
    ]
    try:
        wait(DEMO_PORT, path="/mcp")
        wait(MODEL_PORT)
        wait(PY_PORT)
        wait(GO_PORT)
        mismatches = 0
        tool_diff = False
        for user, chat, text in CASES:
            payload = {"platform_user_id": user, "chat_type": chat, "text": text}
            py_code, py_body = post(PY_PORT, payload)
            go_code, go_body = post(GO_PORT, payload)
            tool_diff = tool_diff or py_body.get("tools_visible") != go_body.get("tools_visible")
            same = py_code == go_code and shape(py_body) == shape(go_body)
            mismatches += 0 if same else 1
            print(f"  [{'same' if same else 'DIFF'}] {user:>4} {chat:<14} {text[:34]:<36} "
                  f"py={py_code} go={go_code}")
            if not same:
                print(f"        python: {json.dumps(shape(py_body), ensure_ascii=False)}")
                print(f"        go    : {json.dumps(shape(go_body), ensure_ascii=False)}")
        print(f"\n{len(CASES) - mismatches}/{len(CASES)} decisions identical")
        if tool_diff:
            print("note: visible tool lists differ by design — the Go gateway lists only tools "
                  "a running MCP service advertises; the Python reference still has a built-in "
                  "registry for services that do not exist yet.")
        return 0 if mismatches == 0 else 1
    finally:
        for proc in procs:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()


if __name__ == "__main__":
    sys.exit(main())
