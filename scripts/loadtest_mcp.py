#!/usr/bin/env python3
"""Load the MCP front with distinct arguments and report what it actually did.

Distinct arguments on purpose: identical ones would be coalesced by single-flight and the
number would describe the cache, not the gateway. Every request is a cache miss by design.

Usage: scripts/loadtest_mcp.py [--url URL] [--token TOKEN] [--levels 1, 5, 10, 25, 50, 100, 200]
"""
import argparse, json, statistics, sys, time, urllib.request
from concurrent.futures import ThreadPoolExecutor

def one(url, token, i, level):
    body = json.dumps({"jsonrpc": "2.0", "id": i, "method": "tools/call",
                       "params": {"name": "jira_search", "arguments": {"query": f"load-{level}-{i}"}}}).encode()
    req = urllib.request.Request(url, data=body, method="POST",
                                 headers={"Content-Type": "application/json",
                                          "Authorization": f"Bearer {token}"})
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            payload = json.loads(r.read())
            ms = (time.perf_counter() - t0) * 1000
            if "error" in payload:
                msg = str(payload["error"].get("message", "error"))[:40]
                return ms, msg
            return ms, ""
    except Exception as e:
        return (time.perf_counter() - t0) * 1000, type(e).__name__[:40]

def pct(xs, p):
    xs = sorted(xs)
    k = max(0, min(len(xs) - 1, int(round((p / 100) * (len(xs) - 1)))))
    return xs[k]

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", default="http://127.0.0.1:8090/mcp")
    ap.add_argument("--token", default="dsh-demo-owner-token")
    ap.add_argument("--levels", default="1, 5, 10, 25, 50, 100, 200",
                    help="comma-separated concurrency levels")
    ap.add_argument("--per-level", type=int, default=0, help="requests per level (default: 8x level)")
    a = ap.parse_args()

    print(f"{'conc':>5} {'req':>5} {'ok':>5} {'err':>4} {'rps':>8} {'p50':>7} {'p95':>7} {'p99':>7} {'max':>7}")
    for level in [int(x) for x in a.levels.replace(" ", "").split(",") if x]:
        n = a.per_level or level * 8
        t0 = time.perf_counter()
        with ThreadPoolExecutor(max_workers=level) as ex:
            res = list(ex.map(lambda i: one(a.url, a.token, i, level), range(n)))
        wall = time.perf_counter() - t0
        ms = [r[0] for r in res]
        reasons = {}
        for _, why in res:
            if why:
                reasons[why] = reasons.get(why, 0) + 1
        errs = sum(reasons.values())
        print(f"{level:>5} {n:>5} {n-errs:>5} {errs:>4} {n/wall:>8.1f} "
              f"{pct(ms,50):>7.1f} {pct(ms,95):>7.1f} {pct(ms,99):>7.1f} {max(ms):>7.1f}")
        if reasons:
            print("      lý do lỗi:", ", ".join(f"{k}×{v}" for k, v in sorted(reasons.items(), key=lambda x: -x[1])))
    print("\nms · rps = requests/second to completion · distinct args ⇒ no coalescing, no cache hits")

if __name__ == "__main__":
    sys.exit(main())
