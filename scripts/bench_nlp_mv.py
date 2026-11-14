"""Compare two ways to turn a question into SQL: our service, and ClickHouse's own aiGenerate.

Both get the same schema text and the same question, against the same database, so the only
difference is who calls the model. Ground truth is hand-written SQL over the aggregate
materialized view (countMerge/sumMerge/avgMerge), because that is the table production reads.

How to run it:

    clickhouse-client --multiquery < scripts/bench_nlp_mv.sql
    PORT=8298 CLICKHOUSE_URL=http://127.0.0.1:8125 CLICKHOUSE_DB=bench \
      MODEL_MCP_URL=http://127.0.0.1:8097/mcp python3 services/clickhouse/server.py
    BENCH_CH=http://127.0.0.1:8125 python3 scripts/bench_nlp_mv.py "label" 3

Trim the raw table between runs to see why the view matters:
    ALTER TABLE bench.events DELETE WHERE ts < '2026-11-13 00:00:00' SETTINGS mutations_sync = 1

Grading is value-based and tolerant of extra columns: "which day" answered with the day and its
count is not wrong.
"""
import json
import os
import sys
import time
import urllib.request

CH = os.environ.get("BENCH_CH", "http://127.0.0.1:8125")
SERVICE = os.environ.get("BENCH_SERVICE", "http://127.0.0.1:8298/mcp")
DB = "bench"

def ch(sql, extra_params=""):
    url = f"{CH}/?database={DB}&default_format=JSON{extra_params}"
    req = urllib.request.Request(url, data=sql.encode(),
                                 headers={"X-ClickHouse-User": "default"})
    started = time.time()
    with urllib.request.urlopen(req, timeout=180) as r:
        doc = json.loads(r.read().decode())
    return doc.get("data") or [], int((time.time() - started) * 1000), doc.get("statistics") or {}

# Ground truth: the view, with the -Merge combinator each state type needs.
QUESTIONS = [
    ("Tổng số event mỗi ngày trong 7 ngày qua",
     "SELECT day, countMerge(events) AS c FROM events_daily GROUP BY day ORDER BY day"),
    ("Trung bình duration_ms của level_complete theo từng platform",
     "SELECT platform, avgMerge(avg_ms) AS a FROM events_daily WHERE event = 'level_complete' GROUP BY platform ORDER BY platform"),
    ("Tổng duration_ms theo từng event",
     "SELECT event, sumMerge(total_ms) AS t FROM events_daily GROUP BY event ORDER BY event"),
    ("Ngày nào có nhiều level_fail nhất",
     "SELECT day FROM events_daily WHERE event = 'level_fail' GROUP BY day ORDER BY countMerge(events) DESC LIMIT 1"),
    ("Số event mỗi platform trong 3 ngày gần nhất",
     "SELECT platform, countMerge(events) AS c FROM events_daily WHERE day >= (SELECT max(day) - 2 FROM events_daily) GROUP BY platform ORDER BY platform"),
    ("Event nào có thời lượng trung bình cao nhất trên android",
     "SELECT event, avgMerge(avg_ms) AS a FROM events_daily WHERE platform = 'android' GROUP BY event ORDER BY a DESC LIMIT 1"),
    ("Tổng cộng có bao nhiêu event",
     "SELECT countMerge(events) AS c FROM events_daily"),
]

def norm(rows):
    """Values, not aliases: two writers naming the same column differently are equally correct.
    A row becomes its sorted tuple of values, and the answer is the sorted list of those rows."""
    out = []
    for r in rows:
        vals = []
        for v in r.values():
            try:
                vals.append(round(float(v), 6))
            except (TypeError, ValueError):
                vals.append(str(v))
        out.append(tuple(sorted(vals, key=str)))
    return sorted(out, key=str)

def covers(rows, gt_rows):
    """Every expected value must appear in some returned row; extra columns are allowed."""
    if not gt_rows:
        return not rows
    want = []
    for r in gt_rows:
        vals = []
        for v in r.values():
            try:
                vals.append(round(float(v), 6))
            except (TypeError, ValueError):
                vals.append(str(v))
        want.append(set(map(str, vals)))
    got = [set(str(round(float(v), 6)) if _num(v) else str(v) for v in r.values()) for r in rows]
    for w in want:
        if not any(w <= g for g in got):
            return False
    return True


def _num(v):
    try:
        float(v)
        return True
    except (TypeError, ValueError):
        return False


def schema_text():
    txt, _, _ = ch("SELECT table, name, type FROM system.columns WHERE database = currentDatabase() ORDER BY table, position")
    return txt

def runs_mv(sql):
    s = (sql or "").lower()
    return "events_daily" in s and ("countmerge" in s or "summerge" in s or "avgmerge" in s)

def prompt_system(_schema_rows=None):
    """The exact schema text the service uses, fetched from its own ch_schema tool.

    Both variants get the same string, so the only difference between them is who calls the
    model — that is the comparison worth making."""
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                       "params": {"name": "ch_schema", "arguments": {}}}).encode()
    req = urllib.request.Request(SERVICE, data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as r:
        schema = json.loads(json.loads(r.read().decode())["result"]["content"][0]["text"])["schema"]
    return ("You translate a question into ClickHouse SQL.\n\nRules, all mandatory:\n"
            "- Output exactly one SELECT statement. No prose, no markdown fences.\n"
            "- Never write, never DDL, never a second statement, never `system.`\n"
            "- Use only the tables and columns below; if the question cannot be answered, output NONE.\n\n"
            "Schema:\n" + schema)


def ask_service(question):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                       "params": {"name": "nlp_ask", "arguments": {"question": question}}}).encode()
    started = time.time()
    req = urllib.request.Request(SERVICE, data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=180) as r:
        doc = json.loads(r.read().decode())
    ms = int((time.time() - started) * 1000)
    res = doc["result"]
    if res.get("isError"):
        return "", [], ms, res["content"][0]["text"]
    b = json.loads(res["content"][0]["text"])
    return b["sql"], b["rows"], ms, ""

def ask_in_db(question, system):
    def lit(s):
        return "'" + s.replace("\\", "\\\\").replace("'", "\\'") + "'"
    sql = ("SELECT aiGenerate('ai_cc', " + lit(question) + ", " + lit(system) + ", 0) AS sql")
    started = time.time()
    rows, _, _ = ch(sql, "&allow_experimental_ai_functions=1&max_execution_time=180")
    gen_ms = int((time.time() - started) * 1000)
    sql_text = (rows[0]["sql"] if rows else "").strip()
    sql_text = sql_text.replace("```sql", "```").split("```")
    sql_text = max(sql_text, key=len).strip() if len(sql_text) > 1 else sql_text[0]
    sql_text = sql_text.rstrip(";")
    if not sql_text or sql_text.upper() == "NONE":
        return "", [], gen_ms, "model said NONE"
    try:
        out, run_ms, _ = ch(sql_text)
    except Exception as exc:
        return sql_text, [], gen_ms, f"execution failed: {str(exc)[:120]}"
    return sql_text, out, gen_ms + run_ms, ""

def main():
    label = sys.argv[1] if len(sys.argv) > 1 else "run"
    reps = int(sys.argv[2]) if len(sys.argv) > 2 else 3
    system = prompt_system()
    raw_n = ch('SELECT count() AS n FROM events')[0][0]['n']
    mv_n = ch('SELECT count() AS n FROM events_daily')[0][0]['n']
    print(f"\n### {label} — raw {raw_n} rows, view {mv_n} rows, {reps} runs per cell")
    print(f"{'question':50} {'variant':8} {'ok':>5} {'median':>7} {'mv':>5}")
    total = {"service": [0, 0], "in-db": [0, 0]}
    lat = {"service": [], "in-db": []}
    mvuse = {"service": 0, "in-db": 0}
    for question, gt_sql in QUESTIONS:
        gt_rows, _, _ = ch(gt_sql)
        for name, fn in (("service", lambda q: ask_service(q)),
                         ("in-db", lambda q: ask_in_db(q, system))):
            oks, times, mv_hits, bad = 0, [], 0, []
            for _ in range(reps):
                sql, rows, ms, err = fn(question)
                times.append(ms)
                if covers(rows, gt_rows):
                    oks += 1
                else:
                    bad.append((sql or err)[:80].replace("\n", " "))
                if runs_mv(sql):
                    mv_hits += 1
            total[name][0] += oks
            total[name][1] += reps
            lat[name].extend(times)
            mvuse[name] += mv_hits
            times.sort()
            med = times[len(times) // 2]
            print(f"{question[:50]:50} {name:8} {oks}/{reps:<3} {med:6} ms {mv_hits}/{reps:<3}")
            if bad:
                print(f"    miss: {bad[0][:110]}")
    print("\nsummary")
    for name in ("service", "in-db"):
        oks, n = total[name]
        times = sorted(lat[name])
        print(f"  {name:8} correct {oks}/{n} ({100*oks//max(n,1)}%) · "
              f"median {times[len(times)//2]} ms · p90 {times[int(len(times)*0.9)-1]} ms · "
              f"used the aggregated table {mvuse[name]}/{n}")


main()
