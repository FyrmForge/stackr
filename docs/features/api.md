# REST API (`/api/v1`)

CLI/automation-grade REST API. Auth via the `x-api-key` header; the live OpenAPI
spec is at `/api/openapi.json` (every route + its scope declared from the same
`op[Req,Resp]()` call, so the spec can't drift from what's enforced).

## Auth & permissions

- **Keys are user-scoped** — each key carries its creator's `UserID`.
- **Capability scopes.** A key is granted a subset of capabilities at creation
  (Settings → API keys). Scope vocabulary (`resource:action`):
  `stacks:read|write`, `apps:read|write`, `apps:deploy`, `vars:read|write`,
  `logs:read`, `dbs:read|write|fork`, `domains:write`, `envs:write`,
  `secrets:read`, `config:read|apply`, `backups:read|write|restore`,
  `storage:read|write`, `tiles:forward` (grouped for the key-creation UI via
  `ScopeGroups`). Three are
  deliberately split out of the write scope next to them, because each does
  something a write does not: `config:apply` applies whatever the file says
  (deletes included), `backups:restore` overwrites live data, and
  `tiles:forward` is unrestricted network access to a container.
- **Bounded by the creator's access.** Write-scopes are only offered/accepted if
  the creator has content-write (owner/member) in their org (or is a server
  admin); read-scopes always available.

**Two enforcement gates** (`internal/stackrd/handlers/api/v1/auth.go`):
1. **Scope gate** — `op()` wraps each route with `requireScope`; a missing scope
   is `403 {"error":{"code":403,"message":"api key missing scope: ..."}}`.
2. **Org/tenancy gate** — `KeyAuth` loads the key's user; every resource is
   checked against the user's live org membership (`requireStackAccess` →
   `404` if the resource's org isn't theirs) and write role
   (`requireOrgWrite` → `403`). Admins bypass org checks.

Both gates are live-evaluated, so revoking a user's membership or write role
immediately narrows their keys.

## Endpoints

| Method + path | Scope |
|---|---|
| `GET /stacks` · `POST /stacks` · `DELETE /stacks/{id}` | stacks:read / :write |
| `GET /stacks/{id}/envs` · `POST /stacks/{id}/envs` | stacks:read / envs:write |
| `GET /apps` · `POST /stacks/{id}/apps` | apps:read / apps:write |
| `GET /apps/{id}` · `PATCH /apps/{id}` · `DELETE /apps/{id}` | apps:read / apps:write |
| `POST /apps/{id}/deploy` | apps:deploy |
| `GET`/`PUT /apps/{id}/variables` | vars:read / vars:write |
| `GET /apps/{id}/deployments` · `GET /deployments/{id}` | apps:read |
| `GET /apps/{id}/logs?tail=N` · `GET /deployments/{id}/logs` | logs:read |
| `GET /dbs` · `GET /dbs/{id}` · `POST /stacks/{id}/dbs` · `DELETE /dbs/{id}` | dbs:read / dbs:write |
| `GET`/`POST /backup-destinations` · `DELETE /backup-destinations/{id}` | backups:read / :write |
| `GET`/`POST /tiles/{id}/backups` · `PATCH`/`DELETE /backups/{id}` | backups:read / :write |
| `GET /backups/{id}/runs` · `POST /backups/{id}/run` | backups:read / :write |
| `POST /backups/{id}/restore` | backups:restore |

The table above is the original core; shipped since, on the same conventions:
config plans (stack + org — `/stacks/{id}/config/plan(s)`,
`/orgs/{id}/config/plan(s)`, approve/reject; `config:read`/`config:apply`),
storage CRUD + sub-paths (`/storage`, `storage:read|write`), domains and
domain-resources CRUD (`domains:write`), volumes CRUD, cron/function one-shot
runs (`POST /apps/{id}/run`), env/stack/org variable pairs, `POST /resolve`
(infra colon-paths), reference catalogue + resolved/unresolved variable views,
port-forward token mint (`GET /tiles/{id}/forward`, `tiles:forward`), and the
server proxy escape hatches (`GET /proxy/config`, `PUT /proxy/static-override`,
`PUT|DELETE /proxy/entries/{name}`). The authoritative list is the live spec
at `/api/openapi.json`.

## Conventions (grounded in Railway + Dokploy research — see below)

- **Bulk env vars.** `PUT /apps/{id}/variables` replaces the whole map (the
  GitOps/CI primitive), mirroring Dokploy's `saveEnvironment`. Not per-key.
- **Explicit deploy.** A var/settings change does **not** auto-redeploy (Railway's
  auto-redeploy is a documented footgun) — call `POST /apps/{id}/deploy`.
- **Logs over REST** — `GET /apps/{id}/logs` (container tail) and
  `/deployments/{id}/logs` (captured build log). This is the gap Dokploy's API
  lacks (their logs are WebSocket-only, issue #3719).
- **API mutations apply immediately** — they do **not** go through the UI staging
  buffer. The API is a trusted automation path; matches how Railway/Dokploy work.

  This holds **even on a stack whose canvas edits stage.** The same field change
  queues into the pending set when a human makes it in the panel and writes
  straight through when a script PATCHes it, so the two surfaces have different
  lifecycles on the same tile. That is deliberate (`plan-parity.md` item 5): the
  pending set exists to batch click-by-click edits before one redeploy, and an
  API call is already an intentional, scripted act. The one thing the API will
  not do is write to a **config-managed** stack at all — `PATCH`, `DELETE` and
  the create routes return 409 there, because the file owns those tiles and the
  next apply would revert anything written behind its back.

- **`PATCH /apps/{id}` writes only the keys you send.** The body is
  `stackconf.TileConf`, so the API and a `stackr-compose.yml` name the same
  fields the same way. Absent and cleared are distinguished by key presence, not
  by value: omit `port` and it keeps its value, send `"port": 0` and it is
  cleared. Env vars, domains, volumes and provisioned slices are **not** in this
  body — each has its own endpoint.

- **`DELETE /dbs/{id}` refuses while consumers hold slices** on the instance
  (409). Dropping it destroys every logical database or bucket cut from it and
  leaves each consumer with a variable that no longer resolves, so the force is
  opt-in: `?force=true`.
- **Uniform error shape** `{"error":{"code","message"}}` on every failure;
  internal errors collapse to a generic message so raw strings don't leak.
- Versioned at `/v1`. No rate-limiting or cursor pagination (single-tenant,
  small lists — neither competitor really paginates either).

## Not covered / deferred

- Streaming/`follow` logs (SSE/WS) — the REST tail covers ~90% of automation.
- Per-domain container ports (config model uses the tile's port).

(The CLI fast-follow shipped: `cmd/stackr` + `internal/cli`, full verb set
over this API. Domain endpoints are wired.)

## Reference

Design informed by a survey of Railway (GraphQL API + `railway` CLI) and Dokploy
(REST + OpenAPI + `x-api-key`, auto-generated CLI). stackr's shape matches
Dokploy's; it borrows Railway's CLI vocabulary and adds the REST logs endpoint
Dokploy lacks.
