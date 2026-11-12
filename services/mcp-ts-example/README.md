# mcp-ts-example — a tool service in TypeScript

One file, no dependencies, no build step (Node 22 runs `.ts` with `--experimental-strip-types`).
The point is not the wiki: it is that **the boundary is the protocol, not the language**. The Go
gateway discovers `wiki_search` the same way it discovers the Go demo service and the two Python
services, and its code contains nothing about wikis.

```bash
PORT=8095 node --experimental-strip-types services/mcp-ts-example/server.ts
curl -s -X POST 127.0.0.1:8095/mcp -H 'Content-Type: application/json' \
  -H 'X-Caller-Id: 1001' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wiki_search","arguments":{"q":"cache"}}}'
```

## What a new capability costs

Two config edits and one process. **Zero lines of gateway code.**

```jsonc
// config/mcp.json  — the allowlist: services this gateway may talk to
{ "name": "wiki-ts", "url": "http://127.0.0.1:8095/mcp", "tools": ["wiki_search"] }

// config/tools.json — the policy facts the service is not allowed to declare about itself
"wiki_search": { "ttl": 600, "description": "Search the team wiki." }

// config/policy.json — and which chats may use it
"chats": { "dm": { "tools": [..., "wiki_search"] } }
```

Verified end to end on 2026-11-12 with all three configs, the demo service and a model service:

```
gateway-go on 127.0.0.1:18090 — 4 tools advertised by 3 service(s), model service: true
{"chat_reply": "Result: runbook-oncall, adr-cache, policy-consent"}
{"chat": "dm", "decision": "answered", "tool": "wiki_search", "user": "1001"}
```

The gateway's only involvement in presentation is a generic `hits` → ids renderer, so a service
that returns `{"hits": [...]}` needs no renderer. Anything more specific belongs in the gateway
on purpose: the chat text is the gateway's product, not the service's.

## What this does not prove

Discovery is a boot-time snapshot: a service that starts after the gateway is invisible until the
gateway restarts. And `X-Caller-Id` is trusted because the service is reachable only from the
gateway (loopback here); across a network that needs mTLS or a per-service token.
