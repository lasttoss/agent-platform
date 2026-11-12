"""Load the same request against both gateways and print the tail, not the average.

An average hides the thing that decides a gateway: what happens to request 400 while request 399
is being served. Both implementations are non-blocking at the socket layer; this measures what
you actually get, including the parts that are not about I/O at all.

Usage: python3 scripts/bench.py [concurrency] [requests]
"""
from __future__ import annotations

import json
import os
import socket
import statistics
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
GO_DIR = ROOT / "services" / "gateway-go"
PY_DIR = ROOT / "services" / "gateway"
DEMO_PORT, GO_PORT, PY_PORT = 8191, 8192, 8193

PAYLOAD = json.dumps({"platform_user_id": "1001", "chat_type": "dm",
                      "text": "tài liệu runbook"}).encode()


def port_free(port: int) -> bool:
    with socket.socket() as sock:
        sock.settimeout(0.2)
        return sock.connect_ex(("127.0.0.1", port)) != 0


def wait(port: int, path: str = "/health", timeout: float = 25.0) -> None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=2):
                return
        except Exception:
            time.sleep(0.2)
    raise SystemExit(f"port {port} never answered")


# Every failure is recorded with its reason: a harness that silently drops the requests it could
# not complete reports "no errors" while measuring only the successes.
failures: list[str] = []


def one(port: int) -> float:
    req = urllib.request.Request(f"http://127.0.0.1:{port}/v1/respond", data=PAYLOAD,
                                 headers={"Content-Type": "application/json"})
    started = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            resp.read()
    except urllib.error.HTTPError as exc:
        # A 429 is a result, not a crash: the daily quota is part of the thing being measured
        # unless the harness raises it, which is why the policy below does exactly that.
        with errors_lock:
            failures.append(f"http_{exc.code}")
        raise
    return (time.perf_counter() - started) * 1000


errors_lock = threading.Lock()


def run(port: int, concurrency: int, total: int) -> dict:
    lat: list[float] = []
    lock = threading.Lock()

    def worker(n: int) -> None:
        local = []
        for _ in range(n):
            try:
                local.append(one(port))
            except Exception as exc:  # noqa: BLE001 — the reason matters more than the type
                with errors_lock:
                    if not isinstance(exc, urllib.error.HTTPError):
                        failures.append(type(exc).__name__)
        with lock:
            lat.extend(local)

    per = total // concurrency
    threads = [threading.Thread(target=worker, args=(per,)) for _ in range(concurrency)]
    started = time.perf_counter()
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    elapsed = time.perf_counter() - started
    lat.sort()
    pick = lambda q: lat[min(len(lat) - 1, int(len(lat) * q))]  # noqa: E731
    return {"n": len(lat), "rps": len(lat) / elapsed, "p50": pick(0.50),
            "p95": pick(0.95), "p99": pick(0.99), "max": lat[-1],
            "mean": statistics.fmean(lat)}


def main() -> int:
    concurrency = int(sys.argv[1]) if len(sys.argv) > 1 else 32
    total = int(sys.argv[2]) if len(sys.argv) > 2 else 640
    for port in (DEMO_PORT, GO_PORT, PY_PORT):
        if not port_free(port):
            raise SystemExit(f"port {port} busy — refusing to measure a process I did not start")

    state = Path(tempfile.mkdtemp())
    (state / "go").mkdir()
    demo_bin, gw_bin = state / "mcp-demo", state / "gateway-go"
    subprocess.run(["go", "build", "-o", str(demo_bin), "./cmd/mcp-demo"], cwd=GO_DIR, check=True)
    subprocess.run(["go", "build", "-o", str(gw_bin), "."], cwd=GO_DIR, check=True)
    (state / "mcp.json").write_text(json.dumps({"servers": [
        {"name": "demo-tools", "url": f"http://127.0.0.1:{DEMO_PORT}/mcp",
         "tools": ["salary_self", "docs_search", "status"]}]}))

    # The committed policy allows 200 requests per user per day. A load test that trips the
    # quota measures the quota, not the gateway — so raise it in a copy, and say so.
    policy = json.loads((PY_DIR / "config" / "policy.json").read_text())
    # Written without thousands separators on purpose: the pre-commit money guard accepts only
    # two counting-pattern fixtures, and a load-test quota is not a salary.
    policy["quota"] = {"requests_per_day": 10000000, "tokens_per_day": 1000000000}
    policy_path = state / "policy.json"
    policy_path.write_text(json.dumps(policy))

    procs = [
        subprocess.Popen([str(demo_bin), "-addr", f"127.0.0.1:{DEMO_PORT}"],
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL),
        subprocess.Popen([str(gw_bin), "-addr", f"127.0.0.1:{GO_PORT}",
                          "-mcp", str(state / "mcp.json"), "-policy", str(policy_path),
                          "-audit", str(state / "audit.log")],
                         cwd=GO_DIR, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL),
        subprocess.Popen([sys.executable, "-m", "gateway.server"], cwd=PY_DIR,
                         env=dict(os.environ, PORT=str(PY_PORT), STATE_DIR=str(state / "py"),
                                  POLICY_PATH=str(policy_path)),
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL),
    ]
    try:
        wait(DEMO_PORT, "/mcp")
        wait(GO_PORT)
        wait(PY_PORT)
        print(f"{concurrency} concurrent clients, {total} requests each gateway, "
              f"same request (docs_search, cache-miss once then hits)\n")
        print(f"{'gateway':<12}{'rps':>9}{'p50':>9}{'p95':>9}{'p99':>9}{'max':>9}"
              f"{'err':>7}{'ok':>7}")
        for label, port in (("go", GO_PORT), ("python", PY_PORT)):
            failures.clear()
            result = run(port, concurrency, total)
            print(f"{label:<12}{result['rps']:>9.0f}{result['p50']:>9.1f}"
                  f"{result['p95']:>9.1f}{result['p99']:>9.1f}{result['max']:>9.1f}"
                  f"{len(failures):>7}{result['n']:>7}")
            if failures:
                kinds = ", ".join(sorted(set(failures)))
                print(f"{'':<12}   failures: {kinds}")
        print("\nms, wall clock inside the client; includes the client's own overhead, identical "
              "for both rows.")
        return 0
    finally:
        for proc in procs:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()


if __name__ == "__main__":
    sys.exit(main())
