# v0 features the rewrite does not have

Status: survey 2026-10-07 (v0 = tag v0.5.0), nothing triaged yet.
**gone** = dropped by a decision, **Later** = parked, **missing** = silently
missing, **partly** = half there. Wanted back so far: port forwarding,
interactive terminal (drawer tab and `stackr ssh`), see `leftovers.md`.

## Silently missing or misleading (look first)
- `proxy_custom`: the admin Caddy tab saves it and says "Saved.", nothing
  reads it (`catalogue.go:182`, `web/handler/admin/handler.go:49`). DECIDE
  127 open.
- Generated secrets (`vars set --generate`, `default: generated` +
  `length:`): REWRITE.md:611 said "as today", but `Param` has type and value
  only (`flow/promote/stackfile.go:44`); an old file with `default:` fails.
- PR env config: `pr_envs:` parsed and ignored (`stackfile.go:114`);
  `runPR` (`internal/service/jobs.go:388`) makes `pr-<n>` for any env on the
  base branch. No enable, TTL, overlay, webhook-secret rotate. PR comments
  and checks Later.
- No job Cancel in the web (`CancelJob` never called under `internal/web`).
- A disabled user cannot be re-enabled (only `DisableUser`).
- Profile edit (name, email) and avatar: fields disabled
  (`account.templ:86-87`).
- Env-wide log stream: placeholder (DECIDE 91), no API route.
- `stack export` (live stack as stackr-compose.yml): no verb.
- `plan preview -f stackr-compose.yml --detailed-exitcode` (CI dry run):
  only `org preview` exists.
- `env reset`: left out, no verb (PROGRESS.md:612-614).
- Run history pruning: no prune code found (not verified).

## CLI
- tile refs, `vars ls` showing which level decided: Later (DECIDE 88/104).
- org defaults: partly, web org drawer and org file only; no API/CLI.
- tile rollback --tag: gone (env-wide by release, DECIDE 161).
- tile metrics/resources: Later. infra fork: Later. infra public: gone.
- image ls/tags/rm, registry set --domain: gone (DECIDE 45), built-in
  registry Later; `admin images` lists but cannot delete.
- `stackr forward`: gone by DECIDE 45, built 2026-10-08.

## UI / canvas
- Search palette and `/`: Later. Notification centre: Later.
- Containers page (all docker containers, system ones too): gone
  (PROGRESS.md:396-398).
- Browser terminal: gone by DECIDE 1, built 2026-10-08 (and `stackr ssh`).
- Metrics charts: Later. SQL row browser, S3 file browser: Later.
- Volume file browser: missing (next item after the migration readiness).
- Env compare panel: gone for v1 (DECIDE 150).
- Commit sha/message on release rows: partly, stored, not shown (DECIDE
  167 open).
- Secret reveal in the web: partly (`params get --reveal` in the CLI).
- Repo/branch picker on tile create: partly, URL text field though
  `ConnectorRepos` exists.
- Host-ports card and port edges: Later (published ports exist).
- Graph groups, undo/redo, right-click menu, flow layout, arrange pref,
  view toggles, brand logos, replica sub-tile detail: gone.
- Log pane level filter, JSON expand, saved prefs: Later.
- Share links (`/s/:token`): Later.

## Proxy / networking
- Trust Cloudflare (fetch their ranges into trusted proxies): not ported.
- Custom TLS certificate upload: gone.
- DNS-01: partly, Cloudflare only at install; v0 took any provider in admin.
  `leftovers.md` "DNS connectors".
- Domain edit in the web (HTTPS, basic auth, headers, websockets, forward
  auth): partly, CLI and stack file only.
- Stack and instance domain resources in the web: partly, org level only;
  API and CLI have all three.

## Deploy / build
- wait-for-ci: Later. Per-tile git tokens, deploy keys, GitLab: Later.
- `moved:` tile renames, `ui_edits: stage`, `allow_overlap`: gone.

## Backups / volumes / managed
- Volume used size and host path: gone (DECIDE 161).
- Backup schedule add/edit/delete in the web: partly, CLI only.
- Volume create in the web: partly, CLI only.
- Managed instance connection block, Database tab, instance domains,
  external port, public slice, fork: gone or Later.
- Volume moves: Later.

## Settings / admin
- Audit log: Later. Servers, multi-node, host stats, join keys, drain:
  Later. Built-in registry: Later.
- Org env colours in the UI: partly, org file only. Custom hex colour: gone.
  Org logo: left out.
- Knobs not ported: `cron_timeout_min` cascade, `cron_run_concurrency`,
  `run_retention_days`, `metric_retention_hours`, `node_group`,
  `build_node`, `volume_move_concurrency` (Later).

## Auth / users
- Member and viewer roles: Later (DECIDE 171).
- Invite emails, resend, expiry days: Later (mail).
- Move a stack between orgs: gone.

## Replaced by an equivalent
deployments by jobs; `vars` by `params`; `storage` by `share` and `host:`
mounts; admin pull registries by org `creds`; `env copy` by `env create` +
`env sync`; stack config plans by releases + promote plan; proxy named
entries by `route` + per-domain raw Caddy; middlewares by domain extras;
auto-domain by `auto:`/`apex:`; cron toggle by `tile pause`; key scopes by
role; org switcher by the home canvas; `update-policy` by image watch +
env `auto:`; admin cleanup by the scheduled cleanup job.
