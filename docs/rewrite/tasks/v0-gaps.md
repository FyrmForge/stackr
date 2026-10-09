# v0 features the rewrite does not have

Status: survey 2026-10-07 (v0 = tag v0.5.0); triaged 2026-10-08 against
d3f0f93 (Fable), see "Triage" at the end.
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

## Triage (2026-10-08, against d3f0f93)
Done since the survey: `proxy_custom` (DECIDE 222), run history pruning
(keep last 50, `leaf/run`), log pane level filter and search, env colours
in the env drawer (palette), `stackr forward`, browser terminal and
`stackr ssh`. `moved:` exists for stacks in the org file, not for tiles.

New, missed by the survey:
- `params.Generate` exists with no callers; a v0 file with `default:` /
  `length:` on a param is refused by strict YAML on first promote.
- `pr_envs:` is parsed and ignored, and `runPR` deploys every PR whose base
  branch has an env: no opt-in, unlike v0 (`enabled: true` needed).

Build order, all nine agreed 2026-10-08 (small unless marked):
1. Honour `pr_envs.enabled`, default off: PR envs are opt-in per stack
   file, as v0 (darthvader 2026-10-08). BUILT: `runPR` reads the stack file
   at the config branch head; `against` and `tiles` still unread.
2. Re-enable a disabled user. Items 2, 4, 5, 6, 7: lead decided, plain
   fixes.
3. Generated secrets: `generate: <length>` on a secret in the stack file
   (made once, never rotated by a redeploy), `params set NAME --generate`
   (agreed 2026-10-08).
4. Job Cancel in the web.
5. Commit sha on release rows (closes DECIDE 167).
6. Org settings API route and CLI (stack and env have one).
7. `cloudflare` keyword in `trusted_proxies`.
8. Per-volume backup schedules in the volume drawer (medium; agreed).
9. `stack export` to stackr-compose.yml, like the org and server exports
   (medium; agreed); the stack-file plan preview for CI later.

Drop: `env reset`, tile rollback --tag, image delete, containers page,
volume create without a tile, tile-level `moved:`, `allow_overlap`, every
unported knob except `cron_timeout_min` (later). Everything else: later.
