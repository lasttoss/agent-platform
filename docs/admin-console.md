# Admin console backend

Login is the company's problem; **what a person may do is the platform's**. This document
covers the second half: an authenticated administrator changes somebody's role, and the change
applies to their next request.

```
transport (token now, SSO later)  →  principal{subject}  →  roleStore.Lookup  →  policy[role]
     who is calling                    who it is             assigned role        what it allows
```

Nothing above changes when SSO arrives: an OIDC-backed authenticator has to produce the same
`subject`, and everything downstream already speaks in subjects.

## Two files, two jobs

| File | Owner | Changes when |
|---|---|---|
| `services/gateway/config/policy.json` | engineers, in a pull request | a role gains or loses a tool |
| `config/roles.json` | administrators, at runtime | a person changes team |

`policy.json` defines what a role *can* do; the role store says *who* has it. Keeping them apart
means a permission change is not a deploy, and a deploy is not a permission change.

## Page

The console is a Vite + React app in `services/console`, built into static files and served by
nginx next to the gateway. nginx also proxies `/admin`, `/stats`, `/health`, `/v1` and `/mcp` to
the gateway, so the browser talks to one origin and there is no CORS rule to relax.

```bash
docker compose -f deploy/compose/docker-compose.yml up --build
open http://127.0.0.1:8088                 # paste a token whose role has admin: true

cd services/console && GATEWAY_URL=http://127.0.0.1:8090 npm run dev   # UI on :5173, proxied
```

The token lives in `sessionStorage`; the page holds no data of its own, so an unauthenticated
GET of the static files reveals nothing. The gateway itself serves only the JSON API — one UI,
in the place where a UI belongs, rather than a second copy embedded in a Go binary.

Measured in a browser at 430 px wide: load → change a role → the row updates, the generation
moves, and the previous holder of that token is refused on the next MCP request.

## API

All endpoints need a bearer token whose role has `"admin": true` in `policy.json`.

```bash
curl -s -H "Authorization: Bearer $TOKEN" localhost:8090/admin/users
curl -s -H "Authorization: Bearer $TOKEN" localhost:8090/admin/roles
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"role":"owner"}' localhost:8090/admin/users/2002/role
```

`GET /admin/users` returns every subject the platform knows (policy users plus console
assignments), with `source: policy|console` and the current `generation`.

## Three rules, enforced server-side

1. **Only an admin role assigns roles.** `admin` is a separate flag from `tools`: seeing every
   tool is not the same permission as handing out permissions.
2. **Nobody changes their own role.** A mis-click must not demote the only administrator.
3. **The last admin cannot be demoted.** With rule 2 in place this is unreachable through the
   API — the actor is always an admin, so if the count is one, the actor is the target and rule 2
   fires first. The check stays as a guard for the day rule 2 is relaxed.

Every change writes an audit line *before* the new state is served:

```json
{"event":"admin_role_change","actor":"1001","actor_role":"owner","subject":"2002",
 "from":"dev","to":"owner","generation":3,"at":"2026-11-14T10:15:10Z"}
```

## Why the generation counter exists

MCP answers are cached by `(tool, args, scope)`. Without a generation, this happens:

```
14:00  dev asks for a report                → cached under scope "role:dev"
14:01  admin removes that tool from role dev
14:02  dev asks again                       → cache hit, data the role may no longer see
```

Cache keys now include the store's generation, so an assignment retires every entry cached
under the old permissions without walking the cache. The counter also moves when a
re-assignment keeps the same role — the point is "permissions were touched", not "role changed".

## Measured

From `scripts/demo_dsh_gateway.sh`, against the running gateway:

```
dev calls jira_create_issue before the change   →  denied: tool_not_in_role
POST /admin/users/2002/role {"role":"owner"}    →  {"from":"dev","to":"owner","generation":3}
dev calls jira_create_issue after the change    →  200, issue created
dev tries to change their own role              →  HTTP 409
audit                                           →  admin_role_change with actor, from, to
```

## What this does not do yet

- No audit screen: the page shows the current state, not the history (the audit log has it).
- No SSO: the authenticator is still the token file. The interface is one function away.
- One role store per process: two gateways sharing a disk are not coordinated.
- The role store is a JSON file, not a table: no history beyond the audit log, and no
  transaction across two changes.
- `GET /admin/users` knows policy users and console assignments, not the HR directory; attaching
  `services/userdir` would make the list authoritative.
