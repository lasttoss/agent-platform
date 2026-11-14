# DeepSeek Harness as the client, the gateway as the boundary

`dsh` (DeepSeek Harness) is a local agent harness: chat UI, sessions, tool registry,
sandbox, approvals. It ships everything a team needs to *use* an agent and nothing a
company needs to *govern* one. This document is the integration that keeps each side doing
its own job: dsh stays the body, the gateway stays the boundary.

## Shape

```
dsh (profile: web)                       gateway-go                         services
┌──────────────────────┐   POST /mcp   ┌────────────────────────┐   JSON-RPC   ┌──────────┐
│ mcp-client plugin    │ ───────────▶  │ bearer token → user    │ ───────────▶ │ jira-mock│
│ tools: mcp__jira__*  │  Authorization│ role from policy.json  │  X-Caller-Id │ (or real │
└──────────────────────┘               │ tools/list filtered    │              │  Jira)   │
                                       │ cache (ttl per tool)   │              └──────────┘
                                       │ audit + /stats/mcp     │
                                       └────────────────────────┘
```

Two rules make it a boundary rather than a proxy:

1. **Identity comes from the transport.** The client sends a bearer token; the gateway maps it
   to a user and reads the role from `policy.json`. Nothing in the JSON-RPC body can name a
   user or a role, so a model cannot choose who it is.
2. **Filtering happens before the model sees anything.** `tools/list` returns only the tools the
   caller's role and consent allow. A tool that is not listed is a tool the model never learns
   exists, which is a different guarantee from "we check at call time".

## Configure dsh

```yaml
# $DSH_HOME/profiles/web/cordis.patch.yml
- insert:
    - id: mcp-jira
      name: '@deepseek-ai/dsh-mcp-client'
      config:
        serverName: jira
        transport: streamable-http
        url: http://127.0.0.1:8090/mcp
        headers:
          Authorization: 'Bearer <per-user token>'
        toolCallTimeoutMs: 15000
```

dsh names the tools `mcp__jira__<tool>`. For a real deployment the token comes from dsh's
credential store (`expandTemplates: true` + `${credential:REF}`), not from a literal.

## Run the evidence

```sh
bash scripts/demo_dsh_gateway.sh      # gateway + jira-mock + curl-level checks
```

What it prints, and why each number matters:

| Observation | What it proves |
|---|---|
| `owner: jira_search jira_get_issue jira_create_issue` / `dev: jira_search jira_get_issue` | the tool list is per-role, produced before the model can ask |
| `denied: tool_not_in_role` for dev → `jira_create_issue` | a call that bypasses the list is still refused, with a reason |
| `cache_hits 1 / cache_misses 1 / cache_hit_rate 0.5` after two identical searches | the TTL in `tools.json` is enforced per tool, not globally |
| `cache_bypass_no_ttl 1` for `jira_create_issue` | a write is never served from cache |
| audit lines `mcp_tools_list`, `mcp_call`, `mcp_denied` | who saw what, who called what, and what was refused |

## Put any OpenAI-compatible model behind dsh

dsh ships DeepSeek-first, but a custom provider is a profile patch, not a fork. This ran against
a relay that serves `deepseek-v4.1-flash` over OpenAI Chat Completions:

```yaml
# $DSH_HOME/profiles/headless/cordis.patch.yml
- id: llm-pi-ai
  config:
    providers:
      commandcode:
        apiKeyEnv: COMMANDCODE_API_KEY     # keep the secret in the environment, not the file
        api: openai-completions
        baseURL: https://api.commandcode.ai/provider/v1
        models:
          - id: deepseek/deepseek-v4.1-flash

- id: agent-default-model
  config:
    provider: commandcode
    model: deepseek/deepseek-v4.1-flash
```

The shipped default route is `deepseek-official`; without the second patch dsh stops with
`MISSING_CREDENTIAL: no API key for provider route "deepseek-official"` even when the relay key
is exported. A provider speaks one protocol, so a relay serving both OpenAI- and
Anthropic-shaped models needs one provider entry per protocol.

## End-to-end, with the real harness

`dsh 0.2.2-rc.1`, profile `web`, `@deepseek-ai/dsh-mcp-client 0.0.1` (vendored), this gateway:

```json
{"event":"mcp_initialize","client":{"name":"dsh-mcp-client","version":"0.0.1"},"protocol":"2025-11-25","role":"owner","user":"1001"}
{"event":"mcp_tools_list","count":3,"role":"owner","tools":["jira_search","jira_get_issue","jira_create_issue"],"user":"1001"}
{"event":"mcp_initialize","role":"dev","user":"2002"}
{"event":"mcp_tools_list","count":2,"role":"dev","tools":["jira_search","jira_get_issue"],"user":"2002"}
```

Same installed harness, different token, different tool surface — decided at the gateway.

## A real agent turn, measured

`dsh --profile headless "<prompt>"` with the relay above, the gateway, and `jira-mock`:

| Run | Token | Result |
|---|---|---|
| "search issues in the platform team, list keys" | owner | answered `JIRA-102, JIRA-103`; audit `mcp_call` miss 30 ms |
| same question again | owner | answered from gateway cache; audit `mcp_call` `cache: hit`, `cache_hit_rate 0.5` |
| "create a jira issue titled 'dev attempt'" | dev | `mcp_tools_list` returned 2 tools; the model answered it has no create tool and made no attempt |

The third row is the point of filtering before the call: an unlisted tool is not a refused call,
it is a call that never happens, so there is no denial to log and nothing for the model to retry.
Direct `curl` against `/mcp` with the dev token still gets `denied: tool_not_in_role`, which is
the second line of defence for a client that ignores `tools/list`.

## Limits (measured, not assumed)

- dsh is `0.2.2-rc.1` and its README says compatibility-breaking changes are coming. The plugin
  version in npm (0.0.1-rc.1) does not match the runtime; installing it from the public registry
  failed here on a missing internal dependency (`@deepseek-ai/dsh-type-meta`). The vendored copy
  inside the dsh package works; the registry path does not.
- Node engine requirement is `>=24.9.0`; this ran on 22.23.1 with an engine warning.
- The gateway negotiates `2025-06-18` while dsh asked for `2025-11-25`. The client accepted the
  lower revision; a future revision may not.
- Multi-user concurrency, SSE streaming responses (this gateway answers plain JSON only), and
  token rotation are still untested.
- The model answered correctly but the harness asked `tools/list` twice in one run; a real
  deployment should treat that as normal startup behaviour, not as a caching bug.
