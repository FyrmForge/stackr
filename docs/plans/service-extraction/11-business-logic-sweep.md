# Business-logic sweep

Why this exists: the three guard tests (`guard/readfree_test.go`,
`guard/writefree_test.go`, `handlers/web/storefree_test.go` — now deleted)
only ever detected calls whose receiver was a `repo.Store`. A handler calling
`h.clus.RemoveVolume(...)` or `h.nodes.AddNode(...)` touches no store method,
so it passed all three while owning the rule outright. The "353 reads left"
number measured database access, not workflow ownership.

This sweep reads every non-generated Go file under `cmd/` and `internal/`
(321 files) and records business logic found outside `internal/stackrd/service/`.

## What counts as business logic

A line is business logic if it **decides something about the domain**:

1. **Branching on domain state** — `if org.ConfigManaged()`, `if len(orgs) <= 1`,
   `if t.Kind == "cron"`. Imports nothing, still a rule.
2. **Validation of domain shape** — name rules, path rules, backend rules,
   "must be absolute", "must stay inside the share".
3. **Orchestration** — more than one service/infra call with a decision between
   them, or a sequence that must not be re-spelled elsewhere.
4. **Direct `infra/` use from a handler** — probing, volume removal, runtime
   calls, engine calls.
5. **Side effects** — notifier fan-out, scheduler reloads, cache invalidation
   fired by a caller rather than the owner.
6. **Duplication across surfaces** — the same rule spelled twice in web and API,
   especially where the two spellings have drifted.

Not business logic: binding params, rendering, HTTP status mapping,
`stackrmw.HTTP(err)`, flash messages, route wiring/DI in `server.go`,
pure string helpers.

## Shards

| Shard | Area | File |
|-------|------|------|
| A | `handlers/web/handler/{project,annotate,deployment,container,db}` | `shards/sweep-a-project.md` |
| B | `handlers/web/handler/{org,server,settings,app,backups,account,auth,cli,notification,search,sharepub,about}` | `shards/sweep-b-panel.md` |
| C | `handlers/web/handler/prhook` + `handlers/web/{graph,components,filebrowse,avatar,wsproxy,wsterm}` + `handlers/web/*.go` | `shards/sweep-c-webinfra.md` |
| D | `handlers/api/v1`, `handlers/api`, `handlers/middleware`, `handlers/stream` | `shards/sweep-d-api.md` |
| E | `cmd/*`, `internal/{installer,installspec,deploystate,netaddr,proxyrelay}` | `shards/sweep-e-boot.md` |
| F | `internal/cli`, `internal/cli/cmd` | `shards/sweep-f-cli.md` |
| G | `internal/stackrd/config/*` | `shards/sweep-g-config.md` |
| H | `infra/{managedtiles,deploy,cigate,jobs,workqueue,volmove,placement,imagewatch}` | `shards/sweep-h-deploy.md` |
| I | `infra/{cluster,runtime,agent,nodes,netpool,envnet,forward,proxy,registry,githubapp,storagetiles,backup,metrics,hostmetrics,mail,gitlog}` | `shards/sweep-i-infra.md` |
| J | `internal/stackrd/service/*`, `store/*`, `envcolor` | `shards/sweep-j-service.md` |

## Findings

Filled in as shards land. See `12-business-logic-plan.md` for the wave plan
built from them.

## Already established before the sweep

Confirmed by audit, not pending:

- **Storage** — probe written 3× (`service/storage.go:101`,
  `handlers/web/handler/server/storage.go:26`, `handlers/api/v1/lifecycle.go:231`);
  consumer check 2× with the API copy missing the org branch
  (`api/v1/storage.go:175`); sub-path volume cleanup owned by neither
  (`server/storage.go:140`, `api/v1/storage.go:184`); `ServerID` defaulting to
  `"local"` (`api/v1/storage.go:104`); config-managed org refusal only on the
  API side (`api/v1/storage.go:196`), so the panel can delete a share a config
  repo owns.
- **Volumes** — `handlers/web/handler/server/handler.go:270-320` owns name
  validation, node resolution, confirm-against-live-list and direct
  `h.clus` create/remove. No `VolumeService` exists.
- **Nodes** — `handlers/web/handler/server/nodes.go:65` calls
  `infra/nodes.AddNode` then separately `service.NodeService.EnsureAgent`.
  One workflow, three layers.
- **Graph** — `project/handler.go:986` applies `slugKey` before
  `GraphService.SavePositions`; `project/stackgraph.go:86` does not. Same
  service method, two node-key meanings. **Drift that already happened.**
  Notifier side effect repeated in 11 handlers (`stackgraph.go:110,122,136,
  148,162,174,188,200,224`, `handler.go:994,1008`).
- **Webhooks** — `prhook/handler.go:39` holds `*deploy.Engine` and no
  `DeployService`. The whole "is this push deployable" rule set lives at
  `:424-434`. Not a straight move: `DeployService.deployable`
  (`service/deploy.go:80`) refuses cron tiles while the webhook path
  deliberately deploys them, and `engine.Park` has no service equivalent.
- **Boot** — `cmd/stackrd/main.go:365` `store.SweepStaleRuns`;
  `cmd/stackrd/seed.go:31-40` is a second thinner copy of
  `DomainResourceService.Create` (`service/domainresource.go:170`);
  `main.go:493-506` branches on `tile.Kind == "function" && tile.RunOnDeploy`
  and calls `store.SetTileImageDigest` in the `OnFinish` callback;
  `cmd/stackrd/ws.go:21,40` does websocket room auth on the raw store.
- **Org delete** — `handlers/web/handler/org/members.go:171-203` owns "org with
  stacks cannot be deleted", "last organization cannot be deleted", "a setup
  draft doesn't count", and post-delete `h.rt.RemoveBuilder`.
  `OrgService.Delete` (`service/org.go:193`) is a one-line store call whose doc
  comment says the rule is "the panel's". Misplaced, **not** bypassable — one
  caller repo-wide, no API route.
- **Backups** — **corrected by the sweep; see `12-business-logic-plan.md` §2 #1.**
  `BackupScheduleService` is *not* a forwarder; it owns tile schedules properly
  (`service/backupschedule.go:198`). This bullet originally filed the panel path
  (`settings/handler.go:240-270`, raw `Save` + manual `ReloadBackups`) as the
  unguarded one. Shards G and J both found otherwise: that path is the panel's
  own **database** backup — `repo.BackupStackr`, a row with no tile, which
  `Create`/`Adopt`/`Reconcile` all bail on (`service/backupschedule.go:289-295`)
  — and it does call `backup.Validate` (`:260`) and `ReloadBackups` (`:267`)
  itself. It is justified.
  G then named **`handlers/api/v1/backups.go:205`** as the real hole: raw
  `schedules.Remove` for a **tile** schedule where `Delete` (`:177`) exists.
  *Re-corrected 2026-09-21:* the next line, `:207`, calls
  `a.sched.ReloadBackups`, so the schedule does **not** keep firing. It is a
  hand-written `Remove` + reload where `Delete` does both — a category-6
  re-spelling, not a live bug. Neither path is a bug.
