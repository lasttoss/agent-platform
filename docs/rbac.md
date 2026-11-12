# RBAC in the gateway

The gateway is the only component that decides who may do what. This page is the reference for
that decision: the gates, in order, the reason codes they produce, and the one endpoint that
answers "why can't the model see this tool?" without reading four files.

## The gates

A tool is visible to the model only if **all** of these hold, and the first one that fails becomes
the reported reason:

| # | Gate | Reason code | Where the fact lives |
|---|---|---|---|
| 1 | the tool has policy facts | `no_policy_facts` | `config/tools.json` |
| 2 | it is not destructive | `destructive` | `config/tools.json` |
| 3 | it is enabled in this chat | `not_in_chat` | `config/policy.json` → `chats[chat_type].tools` |
| 4 | the caller's role grants it | `not_in_role` | `config/policy.json` → `roles` |
| 5 | a running service advertises it | `service_not_advertising` | MCP `tools/list` at boot |
| 6 | a consent grant covers its scope | `no_consent` | `config/consent.json` |
| 7 | the answer may be delivered here | `personal_data_not_here` | `config/tools.json` + `chats[chat_type].personal_data` |

Gate 7 is the only one where *routable* and *visible* legitimately disagree: the tool may run, and
the answer leaves through a DM instead of the chat where the question was asked.

A role and a chat are both allowlists, and the effective grant is their **intersection**:

```
effective = chat allowlist ∩ role grant ∩ consent ∩ advertised ∩ not destructive
```

`"*"` in a role means "as much as this chat already allows" — never more. A role that could widen
an allowlist would be a backdoor with a friendly name, so it cannot.

Two failure modes are refused at load time rather than at request time, because a policy that
silently degrades is worse than a policy that refuses to start:

- a user whose `role` is not in the `roles` table → `policy_invalid`, the gateway does not start;
- a `private_tools` entry that is not marked `personal` → `policy_invalid`.

## Asking the gateway

```bash
curl -s 'http://127.0.0.1:8090/v1/whoami?user=1001&chat=group_public' | jq
```

```json
{
  "user": { "id": "1001", "name": "Dung", "role": "owner", "workspace": "dung",
            "chat_type": "group_public", "personal_data": false, "free_chat": false },
  "quota": { "requests_used": 0, "requests_per_day": 200,
             "tokens_used": 0, "tokens_per_day": 200000 },
  "tools": [
    { "tool": "docs_search", "visible": true,  "routable": true,  "delivery": "chat" },
    { "tool": "drive_search", "visible": false, "routable": false, "blocked_by": "not_in_chat" },
    { "tool": "ocr_extract", "visible": false, "routable": false, "blocked_by": "not_in_chat" },
    { "tool": "salary_self", "visible": false, "routable": false, "blocked_by": "not_in_chat" },
    { "tool": "status", "visible": false, "routable": false, "blocked_by": "not_in_chat" }
  ],
  "counts": { "visible": 1, "routable": 1, "hidden": 4 }
}
```

The endpoint reads only policy, MCP discovery and consent state: no model call, no quota charge,
no cache. It is the answer to a support question, not a feature a user drives.

## Why RBAC is not the only gate

A role answers *who someone is*. It cannot answer three other questions that also decide whether a
tool may run:

| Question | Answered by | Why not a role |
|---|---|---|
| may this person use `salary_self`? | role + chat + consent | yes, RBAC fits |
| may the answer be posted in this group? | `personal` + `personal_data` | a role is about *who*, delivery is about *what the data is* |
| may the gateway read *this person's* Drive? | per-user consent grant | delegated consent is per-user and revocable |
| is the tool available right now? | MCP discovery | a runtime fact, not a permission |

So the model is RBAC for the coarse question plus attributes and consent for the data questions.
Removing either half breaks something real: drop RBAC and every member of a chat gets every tool
of that chat; drop the personal/delivery split and a correct RBAC check still posts a salary into
a group.

## Identity on HTTP

"Identity comes from the transport" is easy to say and easy to fake: the request body carries
`platform_user_id`, so anything that can reach `/v1/respond` can claim to be any provisioned user.
Loopback binding is a network answer to an authorization question.

Set `ADAPTER_TOKEN` and `/v1/respond` and `/v1/whoami` require a matching `X-Adapter-Token`
header (constant-time compare). Unset, behaviour is unchanged — the single-host demo this repo
ships — which is honest about what it is: a demo, not a deployment.

The token is checked by the gateway, never stored in the policy, and never logged. The header it
forwards to services is `X-Caller-Id`, which the gateway sets from the resolved principal: a
client-supplied `X-Caller-Id` is not read at all, so there is nothing to spoof.

## Tests that pin this

- `TestExplainAgreesWithTheRequestPath` — six (user × chat) pairs, comparing the diagnostic path
  against the request path. Two implementations of one decision must not disagree.
- `TestWhoamiNamesTheGateThatBlockedEachTool` — a public group reports `not_in_chat` for the
  salary tool, and a configured-but-absent service reports `service_not_advertising`, not
  "not for you".
- `TestWhoamiReportsTheRoleGate` — a role that does not grant a tool yields `not_in_role`.
- `TestUndefinedRoleRefusesToLoad`, `TestRoleNarrowsTheChatAllowlist` — the load-time refusals.
- `TestAdapterTokenFailsClosed` — no token, wrong token, right token, and the unset case.
