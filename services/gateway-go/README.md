# gateway-go

The gateway. Policy, consent, quota, cache, audit — and an MCP client. That is the whole program.

Everything a caller can *do* is a service behind MCP: the demo tools (`cmd/mcp-demo`, or the
in-process `internal/demotools` package used by tests), the OCR service (`services/ocr`), the
model service (`services/model-mcp`). The gateway contains no extraction code, no provider SDK and
no mail client, so a capability cannot exist without a service to serve it.

```
go test ./...                # 22 tests: real MCP over real HTTP (httptest), no keys
python3 ../../scripts/parity.py   # same 10 requests as the Python reference: 10/10 decisions
```

## The four gates a tool must clear

```
tools/list from a running service   →  config/mcp.json allowlist  →  config/tools.json
   (advertised)                          (this gateway may use it)      (personal? destructive?
                                                                        provider? scope? ttl?)
                                        →  chat allowlist + consent  →  visible to the model
```

Miss any one and the tool is not in the request the model receives. `deploy_prod` is advertised
in a test on purpose: it stays invisible because `tools.json` marks it destructive, and a test
asserts that naming it in a request executes nothing.

## Identity

The gateway resolves the principal from the platform event, then calls the service with
`X-Caller-Id: <user>`. `salary_self` accepts **no user argument**: a model cannot ask for someone
else's record, and there is no code path where it could. Services bind to `127.0.0.1`; a real
deployment adds mTLS or a per-service credential (`config/mcp.json` → `token_env`).

## Known differences from the Python reference, on purpose

| Area | Python reference | Go gateway |
|---|---|---|
| tool implementations | built-in registry | none — MCP services only |
| tools offered | everything in the registry | only what a running service advertises |
| cache storage | SQLite, survives restart | in memory, lost on restart |
| model | OpenAI-compatible HTTP call | the model service over MCP |

The first two are why `scripts/parity.py` compares decisions and *reports* the tool lists instead
of asserting them: the Go list is the honest one, and it changes with what is running.

## Run

```bash
go run ./cmd/mcp-demo -addr 127.0.0.1:8091        # terminal 1 (demo tools)
go run . -addr 127.0.0.1:8090                     # terminal 2 (gateway)
curl -s -X POST 127.0.0.1:8090/v1/respond -H 'Content-Type: application/json' \
  -d '{"platform_user_id":"1001","chat_type":"dm","text":"salary this month"}'
```

Flags: `-policy -consent -tools -mcp -addr -audit`. Defaults resolve from both the service
directory and the repo root; missing config is a startup error, never a silent allow.
