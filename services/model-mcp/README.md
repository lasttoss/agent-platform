# model-mcp

The model, as a service, behind MCP. One tool: `chat_complete(system, user, tools)`.

It answers **which tool fits this sentence** and returns a name plus arguments. It cannot run
anything: the gateway checks the name against `routable_tools` before a single `tools/call`
happens. That split is the point — the model's output is a *proposal*.

Without `MODEL_BASE_URL` it returns an empty turn (`content: ""`, no tool calls, `mock: true`).
A model service that invents a reply when unconfigured is worse than one that says nothing, and
the gateway already has an honest answer for "no tool matched".

```bash
PORT=8097 python3 server.py                       # mock
MODEL_BASE_URL=http://127.0.0.1:4000/v1 MODEL_NAME=claude-sonnet-4-5 PORT=8097 python3 server.py
python3 -m unittest discover -s tests             # 5 tests
```

`tools/call` arguments arrive as a JSON **object** — the OpenAI wire format sends a string, so it
is parsed exactly once, here, rather than leaving both sides to guess.
