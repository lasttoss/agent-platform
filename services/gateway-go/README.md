# gateway-go

The Python gateway, rewritten in Go against the same `policy.json` and `consent.json`.

It exists to answer one question with evidence rather than an opinion: **do the decisions live in
the data and the algorithm, or in the language?** `scripts/parity.py` starts both implementations,
sends the same ten requests and diffs the answers field by field.

```
go test ./...            # 15 tests, no network, no keys
python3 scripts/parity.py   # 10/10 identical against the Python gateway
```

## What is ported

identity → principal (fail closed on unknown user/chat), chat allowlists, consent gating,
destructive-tool removal at the registry, personal-tool privacy in groups, quota, audit log,
the three cache layers (exact / semantic / provider prefix), diacritic folding, and tool calling
with the "only tools the caller may use are ever offered" rule.

## Known differences, on purpose

| Area | Python | Go |
|---|---|---|
| cache storage | SQLite, survives restart | in memory, lost on restart |
| tool implementations | all (OCR worker over HTTP) | `salary_self`, `docs_search`, `status`; the rest return `not_implemented_in_go` |
| model adapter | one call, mock when unset | same contract, mock when `MODEL_BASE_URL` is empty |

The first line is why `scripts/parity.py` runs both servers against fresh state: a warm Python
cache from a previous run is a leftover, not a behavioural difference.

## Run

```bash
go run . -addr 127.0.0.1:8090 -audit state/audit.log
curl -s -X POST 127.0.0.1:8090/v1/respond \
  -H 'Content-Type: application/json' \
  -d '{"platform_user_id":"1001","chat_type":"dm","text":"lương tháng này của tôi"}'
```
