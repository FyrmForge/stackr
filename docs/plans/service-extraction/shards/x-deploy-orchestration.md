# Shard x — deploy orchestration (lens 2)

Packages: `infra/deploy`, `infra/runtime`, `infra/managedtiles`, `infra/envnet`, `infra/jobs`, `infra/workqueue` (+ `cluster`, `placement`, `netpool` where reached directly). Rows are call sites. Paths are under `internal/stackrd/` unless noted. Read-only sweep, 2026-09-18. Extends `00-calibration.md`; the tile-settings `syncProxy` B and the `editGate`/`rejectManaged` A are not repeated.

Surfaces: **web** = `handlers/web`, **api** = `handlers/api/v1`, **cfg** = `config/stackconf` + `config/orgconf` + `config/envops`, **svc** = `service/`.

Legend, ordering column: *before* / *after* / *none* = relative to the surface's own store write in the same func; *n/a* = no store write. Error column: *ret* returned, *log* logged, *ign* discarded (`_ =`), *flash* shown as flash only, *warn* = `warn(...)` collector in apply.go.

## 1. Call-site table

### 1a. `infra/deploy` (Engine + waiting.go)

| # | caller | surface | operation | call | gate | order | err | mode |
|---|---|---|---|---|---|---|---|---|
| D1 | `web/handler/deployment/handler.go:84` | web | deploy tile | `engine.Enqueue(app,"manual")` | `Kind=="cron"` → 400 (`:78`); `envnet.UpperEnv` → 400 (`:81`) | n/a | ret | engine worker |
| D2 | `api/v1/apps.go:488` | api | deploy tile | `engine.Enqueue(t,"api")` | `envnet.UpperEnv` → 400 (`:485`); **no cron check** | n/a | ret | engine worker |
| D3 | `config/stackconf/apply.go:89,92,94` | cfg | deploy tile (apply) | `Enqueue` / `EnqueueCurrent` / `EnqueuePromote` by `isUpper`/`ownRepo` (`:86-96`) | none; cron/function deploy same build (`:1237`) | after CreateTile/UpdateTile | warn | engine worker, inside apply job |
| D4 | `config/stackconf/apply.go:141` | cfg | promote rest | `EnqueuePromote(t,commit)` | git tiles not already `deployed` | n/a | warn | engine worker |
| D5 | `web/handler/deployment/handler.go:101` | web | rollback | `EnqueueRollback(app,tag)` | `image_tag` non-empty; **no UpperEnv gate** | n/a | ret | engine worker |
| D6 | `api/v1/lifecycle.go:111` | api | rollback | `EnqueueRollback(t,tag)` | `image_tag` non-empty; no UpperEnv gate | n/a | ret | engine worker |
| D7 | `web/handler/deployment/handler.go:135` | web | cancel deploy | `engine.Cancel(id)` | loadDeployment only | n/a | void | inline |
| D8 | `api/v1/lifecycle.go:124` | api | cancel deploy | `engine.Cancel(d.ID)` | loadDeployment only | n/a | void | inline |
| D9 | `web/handler/app/handler.go:799` | web | attach/detach volume tile | `Enqueue(target,"volume")` for old and new target | `editGate` only if attach/path changed (`:738-742`); target must be `service` && !managed (`:747`) | after `UpdateTile` (`:791`) | ign | engine worker |
| D10 | `web/handler/project/handler.go:616` | web | create volume tile with attach | `Enqueue(target,"volume")` | `ConfigManaged` → 409 (`:603`) | after `CreateTile` (`:607`) | ign | engine worker |
| D11 | `api/v1/volumes.go:81` | api | create volume tile | `Enqueue(app,"volume")` | (gate not checked above `:55`) | after `CreateTile` (`:78`) | **ret** | engine worker |
| D12 | `api/v1/volumes.go:106` | api | delete volume tile | `Enqueue(app,"volume")` when `AttachedTileID!=""` | `rejectManaged` (`:97`); **attached allowed** | after `teardownTile` (`:100`) | ign | engine worker |
| D13 | `web/handler/app/storage.go:109,136` | web | attach/detach storage line | `Enqueue(a,"storage")` if `Status=="running"` | attach: `storagetiles.ValidateAttach`, dup check; detach: `RequireUnmanaged` (`:122`) | after `UpdateTile` | ign | engine worker |
| D14 | `web/handler/app/handler.go:890` | web | provision / attach provision → wire env | `Enqueue(a,"provision")` | only when `uiManaged` && varName!="" (`:874-884`) | after `UpdateTile` (`:887`) | ret | engine worker |
| D15 | `api/v1/databases.go:388,405` | api | provision / attach provision → wire env | `Enqueue(consumer,"provision")` | skip when `ConfigManaged` (`:378`); `AutoInjectAll` engines inject all vars (`:381-389`), else single ref when envVar!="" | after `UpdateTile` | ret | engine worker |
| D16 | `web/handler/app/handler.go:1202` | web | save tile variables | `Enqueue(a,"variables")` | `Kind=="service" && !managed && !volume && Status=="running"` (`:1201`) | after `UpdateTile` (`:1195`) | ret | engine worker |
| D17 | `web/handler/prhook/handler.go:400,405` | web (webhook) | push autodeploy | `Park(t,"push",sha)` if `WaitForCI && ConnectorID!=""`, else `Enqueue(t,"push")` | default static env, git, service/cron, branch+repo match, `watchMatch`, not in `done` | n/a | ign (counted) | engine worker |
| D18 | `web/handler/prhook/handler.go:685` | web (webhook) | PR sync redeploy | `Enqueue(&t,"webhook")` | git && service/cron | n/a | ign | engine worker |
| D19 | `web/handler/app/handler.go:427` | web | list branches | `engine.ListBranches` | — | n/a | n/a | inline (git) |
| D20 | `web/handler/deployment/handler.go:192` | web | deploy log | `engine.LogPath` + `os.ReadFile` | — | n/a | ign | inline |
| D21 | `api/v1/logs.go:121` | api | deploy log | `engine.DeployLog` | — | n/a | ign | inline |
| D22 | `web/handler/project/handler.go:930` | web | set stack secret (config prompt) | `deploy.ClearWaiting(name,stackID)` | value non-empty (`:919`) | after `UpsertVariable` | void | inline; then `planner().RunAll` sync (`:933`) |
| D23 | `web/handler/project/handler.go:2302` | web | set stack variable | `deploy.ClearWaiting` | `secretNameRe` | after `UpsertVariable` | void | inline; `replanAsync` (`:2303`) |
| D24 | `web/handler/project/handler.go:2602` | web | set env variable | `deploy.ClearWaitingEnv` | `secretNameRe` | after `UpsertVariable` | void | inline; `replanAsync` |
| D25 | `web/handler/org/config.go:180` | web | set org secret (config prompt) | `deploy.ClearWaitingOrg` | value non-empty | after `UpsertVariable` | void | inline; `orgcfg.Plan` (`:184`) |
| D26 | `web/handler/org/settings.go:442` | web | set org variable | `deploy.ClearWaitingOrg` | `varNameRe` (`:425`) | after `UpsertVariable` | void | inline; **no replan** |
| D27 | `api/v1/variables.go:176-180` | api | set variables (bulk, tile/stack/env/org) | `ClearWaitingEnv/ClearWaiting/ClearWaitingOrg` by ownerKind | `Generate` skips existing (`:153`) | after each `UpsertVariable` (`:167`) | void | inline; **no replan** |
| D28 | `config/stackconf/apply.go:1856,1870` | cfg | apply vars | `ClearWaiting` / `ClearWaitingEnv` | — | after write | void | inside apply job |
| D29 | `config/orgconf/orgconf.go:711,728` | cfg | org apply vars + generated secrets | `ClearWaitingOrg` | `Default=="generated" && !set` | after `UpsertVariable` | void | inline |
| D30 | `web/graph/graph.go:669`, `config/stackconf/apply.go:1362` | web/cfg | read waiting reason | `deploy.WaitingFor(status)` | — | n/a | n/a | read |
| D31 | `web/handler/org/members.go:214` | web | delete org | `rt.RemoveBuilder(deploy.BuilderFor(slug))` | last-org guard (`:205`) | after `DeleteOrg` (`:209`) | log | inline |

### 1b. `infra/managedtiles`

| # | caller | surface | operation | call | gate | order | err | mode |
|---|---|---|---|---|---|---|---|---|
| M1 | `api/v1/databases.go:241,247,248-260` | api | create managed instance | `NewDB` → `CreateTile` → `PublishConnection` → `svc.Deploy` → `UpdateTileStatus running/error` | `managedGuard(s)` (`:213`), `requireOrgWrite`, slug checks | Deploy after CreateTile | Deploy err → status "error", log; 201 either way | **inline, request ctx** |
| M2 | `web/handler/project/handler.go:1703,1722` | web | create managed instance | `NewDB`; then either `stageChange(OpCreate)` (`:1710`, `!ConfigManaged`) or `managedErr` (`:1716`) | `!ConfigManaged` → **always stages**; `:1719-1729` (`CreateTile`+`PublishConnection`, no Deploy) unreachable | — | ret | staging → apply job |
| M3 | `config/stackconf/apply.go:1197,1220,1227` | cfg | create managed instance | `NewDB` → `CreateTile` → `ReplaceTileVars` → `syncBindings` → `PublishConnection` → `syncDomains` → `DBs.Deploy` → status | apply gate | Deploy after row | warn + status "error" | inside apply job |
| M4 | `config/orgconf/orgconf.go:967-990` | cfg | org shared instance create/replace/remove | `NewDB` → `CreateTile` → `PublishConnection` → `DBs.Deploy` → status; replace = `DBs.Remove` + `DeleteTile` + recreate; undeclared = `DBs.Remove` + `DeleteTile` | declared in `f.Shared` | after row | `failed` list; Remove ign | inline |
| M5 | `config/envops/envops.go:81,104` | cfg (web+api via `ops()`) | clone env tiles | `NewDB` (fresh creds) → `CreateTile` → `PublishConnection`; **no Deploy** | — | after row | ret | inline |
| M6 | `web/handler/db/handler.go:251` | web | deploy managed instance | `dbs.Deploy` → `UpdateTileStatus` → `statusChanged` | none | n/a | log + notifier | **goroutine, `context.Background()`** |
| M7 | `web/handler/db/handler.go:273` | web | stop managed instance | `dbs.Stop` → `UpdateTileStatus stopped` → notifier | none | Stop before status | log | inline |
| M8 | `web/handler/db/handler.go:291` | web | start managed instance | `dbs.Start` (deploys if no service) → status → notifier | none | before status | log | inline |
| M9 | `web/handler/db/handler.go:520` | web | managed settings (port/limits/image/policy) | `UpdateTile` → `dbs.Deploy` if `running` | `requireFileUnowned` (`:483`) | Deploy after UpdateTile | ret (row already saved) | inline, request ctx |
| M10 | `api/v1/databases.go:189` | api | managed settings patch | `UpdateTile` → `NewService().Deploy` if `redeploy && running` | `rejectManaged` (`:112`); `redeploy` only when a container-borne field changed (`:120-160`) | after UpdateTile | 422 with "settings saved but redeploy failed" | inline, request ctx |
| M11 | `config/stackconf/apply.go:1473`, `config/orgconf/orgconf.go:942` | cfg | managed settings via apply | `DBs.Deploy` if `running`/`error` && `needsDBRedeploy(fields)` | apply gate | after UpdateTile | apply: returns err + status "error"; orgconf: `failed` list | inside apply |
| M12 | `config/stackconf/apply.go:603` (`restartTile`) | cfg | rename redeploy | `DBs.Deploy` if `running`/`error` | — | after rename | warn + status | inside apply |
| M13 | `web/handler/db/handler.go:464` | web | delete managed instance | `dbs.Remove` (RemoveService + `netpool.ReleaseDB`) → `DeleteTile` | `requireFileUnowned`, `RequireConfirm`, **refuses if any provisions** (`:457`); **no `px.RemoveApp`** | Remove before DeleteTile | ret | inline |
| M14 | `api/v1/databases.go:437` (`teardownTile`) | api | delete managed instance | `envnet.TearDown` + `px.RemoveApp` + `DeleteTile`; **never `dbs.Remove` / `netpool.ReleaseDB`** | `rejectManaged`; provisions block unless `?force=true` (`:424`) | before DeleteTile | ret | inline |
| M15 | `web/handler/app/handler.go:917` | web | provision slice for consumer | `dbsvc().Provision(instance,a,"",false)` | `rejectManaged` (`:906`); `!managed && !volume`; `Eligible` | n/a (Provision writes row) | flash | inline |
| M16 | `api/v1/databases.go:310` | api | provision slice for consumer | `Provision(instance,consumer,in.Name,in.Public)` | `!managed && !volume`; `ResolveTarget` scoped or `GetTile`; `Eligible`; **no `rejectManaged`** (`:275-283`) | n/a | 400 | inline |
| M17 | `web/handler/app/handler.go:951` | web | attach existing slice | `AttachExisting` | `rejectManaged` (`:936`); same env | n/a | flash | inline |
| M18 | `api/v1/databases.go:343` | api | attach existing slice | `AttachExisting` | same env; **no `rejectManaged`** | n/a | 400 | inline |
| M19 | `web/handler/app/handler.go:1000` | web | detach slice | `Detach(p)` | `rejectManaged`; consumer match | n/a | ret | inline; **no redeploy** |
| M20 | `api/v1/lifecycle.go:267` | api | detach slice | `Detach(p)` | `rejectManaged`; consumer match | n/a | ret | inline; no redeploy |
| M21 | `web/handler/app/handler.go:1636` | web | delete tile → orphan slices | `Detach` per consumer provision | inside Delete | before `DeleteTile` | ign | inline |
| M22 | `config/stackconf/apply.go:1562` | cfg | delete tile → orphan slices | `Detach` per provision | apply | before `DeleteTile` | warn | inside apply |
| M23 | `api/v1/apps.go:472` (`teardownTile`) | api | delete tile | **no Detach**: `envnet.TearDown` + `RemoveApp` + `DeleteTile` | `rejectManaged` | — | ret | inline |
| M24 | `web/handler/db/handler.go:359` | web | drop slice (all consumers) | `DropDB(d,dbName)` | none (form value) | n/a | flash | inline |
| M25 | `api/v1/resolve.go:166` | api | drop slice | `DropDB(inst,p.DBName)` | `requireTile(inst, write)` `:162`; no `rejectManaged` | n/a | ret | inline |
| M26 | `web/handler/db/handler.go:386` | web | fork slice | `ForkSlice(d,src,"","")` | `src.InstanceTileID==d.ID` | n/a | flash | inline |
| M27 | `api/v1/resolve.go:142` | api | fork slice | `ForkSlice(inst,src,in.Slug,in.Name)` | `requireTile(inst, write)` `:138`; no `rejectManaged` | n/a | ret | inline |
| M28 | `api/v1/resolve.go:116` | api | provision slice by infra path | `ProvisionSlice(inst,envID,slug,name,public)` | `ServesEnv` (`:106`) | n/a | ret | inline |
| M29 | `config/stackconf/slices.go:85-95` | cfg | declare slice | `WaitReady(90s)` → `ProvisionSlice` or `CloneSlice` (ephemeral) → `stampSlicePolicy` | `Eligible` (`:79`) | n/a | ret | inside apply |
| M30 | `web/handler/db/handler.go:429` | web | set bucket public | `SetBucketPublic` per row sharing `DBName` | none | n/a | flash | inline |
| M31 | `api/v1/lifecycle.go:292` | api | set bucket public | `SetBucketPublic` per `provisionRows` | none | n/a; `p.Public` set in memory only (`:297`) | 400 | inline |
| M32 | `config/stackconf/slices.go:112` | cfg | set bucket public | `SetBucketPublic` if `fields["public"]` | — | n/a | ret | inside apply |
| M33 | `config/stackconf/slices.go:147,149` | cfg | remove slice | `DropDB` if `OnRemove=="drop"` else `Detach` | — | n/a | ret | inside apply |
| M34 | `config/envops/envops.go:533,537,624,628` | cfg (web+api) | env teardown → slices | `Detach` (static) / `DropDB` (ephemeral) | `env.Type` | before `DeleteEnvironment` | ign | inline |
| M35 | `config/envops/envops.go:174,250` | cfg | clone env → slices | `svc.Provision` per base provision, `repointRefs` | — | after CreateTile | log | inline |
| M36 | `web/handler/db/handler.go:201`, `:108`, `data.go:116-220`, `files.go:35-105` | web | reads: `ServiceName`, `DataVolume`, PG browse, S3 browse | — | — | n/a | ret | inline |
| M37 | `web/handler/app/handler.go:830-876`, `api/v1/databases.go:68,262,391`, `resolve.go:38-180`, `web/handler/db/handler.go:334,554,581`, `config/stackconf/runner.go:937`, `plan.go:794`, `slices.go:29` | all | pure helpers: `Ref`, `DefaultOutput`, `InfraPath`, `ResolveTarget`, `ResolveInfraPath`, `ResourceSlug`, `ScopeLabel`, `SpeaksHTTP`, `SliceName`, `EligibleInstances`, `AttachableDBs`, `Engines[...]` | — | n/a | — | read |
| M38 | `api/v1/envs.go:195`, `web/handler/project/handler.go:2641`, `web/handler/app/handler.go:1010` | web/api | construct `managedtiles.NewService(clus,store)` per request | — | — | — | — | — |

### 1c. `infra/envnet`

| # | caller | surface | operation | call | gate | order | err | mode |
|---|---|---|---|---|---|---|---|---|
| E1 | `web/handler/app/handler.go:335` | web | stop tile | `clus.ScaleService(envnet.ServiceFor(a),0)` → `UpdateTileStatus stopped` → `statusChanged` | none | before status | scale: ret; status: log | inline |
| E2 | `api/v1/lifecycle.go:36` | api | stop tile | same scale → `UpdateTileStatus` | none | before status | both ret; **no notifier** | inline |
| E3 | `web/handler/app/handler.go:380-387` | web | restart tile | `ServiceFor` → `clus.RestartService(name,a.Replicas)` → status running/stopped → notifier | `name==""` → 400 | before status | ret; status log | inline |
| E4 | `api/v1/lifecycle.go:54-58` | api | restart tile | same | same | before status | ret; status ret/log; no notifier | inline |
| E5 | `web/handler/app/handler.go:1632` | web | delete tile | `envnet.TearDown` → `px.RemoveApp` → `Detach`× → `DeleteTile` | `editGate` (stage) (`:1618`); volume attached → 400 (`:1622`); `RequireConfirm` | before DeleteTile | void/ign | inline |
| E6 | `api/v1/helpers.go:130` (`teardownTile`; callers `apps.go:472`, `volumes.go:100`, `databases.go:437`) | api | delete tile/volume/db | `envnet.TearDown` → `px.RemoveApp` → `DeleteTile` | `rejectManaged` at each caller; **no confirm, no attached-volume refusal, no Detach** | before DeleteTile | void/ign; DeleteTile ret | inline |
| E7 | `config/stackconf/apply.go:621` (`stopContainers`, from `deleteTile:1552`) | cfg | delete tile | `TearDown` → `RemoveApp` → `Detach`× → `DeleteTile` | apply | before | warn | inside apply |
| E8 | `config/envops/envops.go:517` | cfg (web `project/handler.go:392,432`, api `envs.go:80,107`, prhook `:698`) | env teardown / reset / PR close | per tile: `TearDown` + `RemoveApp` + `reclaimSlices`; then env slices; `netpool.ReleaseDB` per shared instance (`:576`), `netpool.ReleaseEnv` (`:580`); `DeleteEnvironment` | web delete: `ConfigManaged`→409, `len(envs)>1`, `RequireConfirm`; api delete: `rejectManaged`, `len>1`, `envRunning` unless `force`; reset: `ConfigManaged` required both | before | ign; netpool ret | inline |
| E9 | `config/stackconf/moved.go:280,306`, `config/orgconf/moved.go:93` | cfg | rename env / rename tile | `TearDown` each tile → rename row → `restartTile` | plan move | before | void | inside apply |
| E10 | `api/v1/apps.go:485`, `web/handler/deployment/handler.go:81`, `web/components/panel.go:74` | web/api | upper-env deploy gate / panel flag | `envnet.UpperEnv` | — | n/a | — | read |
| E11 | `api/v1/logs.go:29`, `web/handler/app/handler.go:114`, `web/handler/project/handler.go:1200`, `api/v1/forward.go:50,60,64`, `web/components/panel.go:80`, `web/handler/project/placement.go:48`, `web/handler/app/handler.go:852`, `config/stackconf/runner.go:925`, `config/varref/varref.go:782` | all | reads: `ServiceFor`, `Resolve`, `Net`, `TileAlias` | — | n/a | — | read |

### 1d. `infra/jobs`

| # | caller | surface | operation | call | gate | order | err | mode |
|---|---|---|---|---|---|---|---|---|
| J1 | `web/handler/app/handler.go:201` | web | run now | `jobs.StartApp(id,TriggerManualWeb,actor)` | **none** (any kind) | n/a | ret | workqueue (`jobs.go:344`) |
| J2 | `api/v1/apps.go:512` | api | run now | `StartApp(id,TriggerManualAPI,keyName)` | `Kind in {cron,function}` → else 400 (`:501`); `jobs==nil` → 503 | n/a | ret | workqueue |
| J3 | `web/handler/app/handler.go:221` | web | stop run | `jobs.Stop(runID)` | `run.Ref=="app:"+id` | n/a | flash | inline |
| J4 | `api/v1/apps.go:535` | api | stop run | `jobs.Stop` | same + `jobs==nil` 503 | n/a | JSON bool | inline |
| J5 | `web/handler/app/handler.go:410` | web | toggle cron | `UpdateTileStatus paused/idle` → `LoadSchedules` → notifier | **no kind check** | after | ign | inline |
| J6 | `api/v1/lifecycle.go:93` | api | toggle cron | same | `Kind=="cron"` → else 400 (`:79`) | after | ign; no notifier | inline |
| J7 | `web/handler/app/handler.go:1592` | web | save tile settings | `LoadSchedules` if `Kind=="cron"` | `editGate`; `ValidateCron` (`:1449`) | after UpdateTile | ign | inline |
| J8 | `api/v1/apps.go:458` | api | patch tile | `LoadSchedules` if cron | `rejectManaged`; `ValidateCron` (`:404`) | after UpdateTile | ign | inline |
| J9 | `web/handler/project/handler.go:611` | web | create tile | `LoadSchedules` if cron | `ConfigManaged`→409; `ValidateCron` (`:495`) | after CreateTile | ign | inline |
| J10 | `api/v1/apps.go:162` | api | create tile | `LoadSchedules` if cron | `ValidateCron` (`:115`) | after CreateTile | **ret** (row exists, 500) | inline |
| J11 | `api/v1/apps.go:475` | api | delete tile | `LoadSchedules` if cron | `rejectManaged` | after teardown | ign | inline |
| J12 | `web/handler/app/handler.go:1612-1641` | web | delete tile | **no `LoadSchedules`** for cron | — | — | — | — |
| J13 | `web/handler/project/handler.go:353,392,432`, `envcompare.go:146` | web | env clone / delete / reset / copy tile | `LoadSchedules` | — | after | ign | inline |
| J14 | `api/v1/envs.go:84,111,151` | api | env delete / reset / copy | `LoadSchedules` | `jobs!=nil` | after | ign | inline |
| J15 | `web/handler/prhook/handler.go:534,702` | web (webhook) | PR env open / close | `LoadSchedules` | — | after | ign | inline |
| J16 | `config/stackconf/apply.go:862-863` | cfg | apply | `Jobs.Hold(stackID)` / `defer Release` | — | around walk | — | inside apply |
| J17 | `config/stackconf/apply.go:1100` | cfg | apply | `LoadSchedules` if `cronTouched` | — | after walk | warn | inside apply |
| J18 | `config/stackconf/apply.go:1202`, `stackconf.go:947,1079` | cfg | validate | `jobs.ValidateCron` | — | before CreateTile | ret | inline |
| J19 | `web/handler/app/handler.go:272` | web | runs log stream | `jobs.RunService(a,runID)` | — | n/a | — | read |

### 1e. `infra/workqueue`

| # | caller | surface | operation | call | gate | dedupe key | err |
|---|---|---|---|---|---|---|---|
| W1 | `web/handler/project/handler.go:793` | web | approve plan | `stackconf.EnqueueApply(q,p,cp,force=true)` | (plan shard) | `cp.ID` | ret |
| W2 | `api/v1/config.go:182` | api | approve plan | `EnqueueApply(..., force=true)` | (plan shard) | `cp.ID` | ret |
| W3 | `web/handler/prhook/handler.go:344` | webhook | push plan | `EnqueueApply(..., force=false)` | — | `cp.ID` | ret |
| W4 | `web/handler/project/releases.go:322` | web | promote with plan | **raw `work.Enqueue(ApplyKind, p.ID, ApplyJob{Force:true, PromoteEnv, PromoteCommit})`** | `promoteEnv`, commit non-empty | **`p.ID` (stack)** | flash |
| W5 | `api/v1/releases.go:158` | api | promote with plan | **raw `work.Enqueue(ApplyKind, s.ID, ApplyJob{Force: in.Force, ...})`** | `promoteEnv`; `work==nil`→503 | **`s.ID` (stack)** | ret |
| W6 | `web/handler/project/releases.go:335`, `api/v1/releases.go:146` | web/api | promote without plan | `applier.Promote(...)` inline, request ctx | — | none | flash / 400 |
| W7 | `config/stackconf/job.go:38-125` | cfg | apply job handler | `RegisterApply`, `RunApplyJob`, `EnqueueApply` (dedupe `cp.ID`, doc at `:108-111` says "the one way") | — | — | — |
| W8 | `infra/jobs/jobs.go:344` | (infra) | cron run | `work.Enqueue(jobs.Kind, run.ID, ...)` | — | run id | ret |

### 1f. `infra/cluster` / `infra/runtime` / `placement` / `netpool` reached directly (mutations only; reads not rowed)

| # | caller | surface | operation | call | gate | err |
|---|---|---|---|---|---|---|
| C1 | `web/handler/server/handler.go:269` | web | create raw docker volume on node | `clus.CreateVolume(node,name)`; **no store row** | `ValidVolumeName`, node joined | ret |
| C2 | `web/handler/server/handler.go:303` | web | delete raw docker volume | `clus.ListVolumes` → `RequireConfirm` → `clus.RemoveVolume` | confirm against live name; **no in-use check** | flash |
| C3 | `web/handler/server/storage.go:33,117,193`, `api/v1/storage.go:136,164,224`, `api/v1/lifecycle.go:315` | web/api | storage probe / delete / delete sub-path | `storagetiles.Probe` + `clus.RemoveVolume(StorageVolume(id))` | web: `attachedConsumers`; api: `ParseAttachment` scan (two implementations of "still attached") | ign |
| C4 | `web/handler/container/handler.go:203,215,236` | web | start/stop/remove container | `clus.StartContainer` / `StopContainer` / `StopRemove` | `ContainerIsSystem` on stop/remove (running only) | ret; notifier |
| C5 | `web/handler/server/nodes.go:319,347,350,385` | web | node availability / remove / group | `rt.SetNodeAvailability`, `rt.RemoveNode`, `rt.SetNodeGroup` | manager refused; `confirmed(name)` | ret |
| C6 | `web/handler/server/nodes.go:120` | web | join script | `rt.JoinToken` | `nodes.Redeem` | ret |
| C7 | `web/handler/app/handler.go:490,1545`, `server/nodes.go:427`, `project/handler.go:981`, `config/stackconf/apply.go:1938` | web/cfg | placement reads | `placement.NodeOf`, `IsPinned`, `For`, `InGroup` | — | — |
| C8 | `config/envops/envops.go:576,580` | cfg | env teardown | `netpool.ReleaseDB`, `netpool.ReleaseEnv` | `SharedNetName!="" && ScopeKind!="org"` | ret |
| C9 | `service/admin.go:145,156` | svc | panel upgrade | `rt.TagImage`, `rt.UpdateServiceImage(..., &swarm.UpdateConfig{...})` | — | ret |

`runtime` symbol use in handlers otherwise (non-templ): `runtime.Runtime` type 16×, `NormalizeRestart` 6, `Node` 6, `ParseDevice` 3, `ParseFileMount` 3, `LabelApp` 3, `LabelDB` 2, `NodeTaskInfo` 2, `ContainerDetail` 2, `ScaleService`/`ProxyRelayImage`/`PanelAlias`/`VolumeInfo`/`ManagedContainer`/`ContainerStats` 1 each. Templ files (`*_templ.go`, `.templ`) import `runtime` for `FmtBytes` and view types; not rowed.

### 1g. Class D

| # | file | import |
|---|---|---|
| X1 | `service/admin.go:16` | `github.com/docker/docker/api/types/swarm` — used at `:156-161` for `swarm.UpdateConfig`, `swarm.UpdateOrderStopFirst`, `swarm.UpdateFailureActionRollback`. Only hit above `infra/` (grep over `internal/stackrd` excluding `infra/`, non-test). |

## 2. Orchestration sequences

### S1. Delete tile (service/cron/function)

| step | web `app/handler.go:1612` | api `apps.go:463` → `helpers.go:129` | cfg `apply.go:1543` |
|---|---|---|---|
| gate | `editGate` (may stage) `:1618` | `rejectManaged` `:468` | plan |
| confirm | `RequireConfirm(slug)` `:1626` | — | — |
| `envnet.TearDown` | `:1632` | `:130` | `:1552`→`:621` |
| `px.RemoveApp` | `:1633` | `:132` | `:1554` |
| `Detach` provisions | `:1636` | **missing** | `:1562` |
| `DeleteTile` | `:1638` | `:134` | `:1567` |
| `jobs.LoadSchedules` (cron) | **missing** | `:475` | `:1100` |

Class B ×2: API leaves consumer provisions pointing at a deleted tile (`ListProvisionsByConsumer` rows survive); web leaves a deleted cron in the in-memory schedule until the next reload.

### S2. Delete volume tile

| step | web (same `Delete` as S1) | api `volumes.go:88` |
|---|---|---|
| gate | `editGate`; `AttachedTileID!=""` → 400 `:1622` | `rejectManaged` `:97`; attached allowed |
| teardown + row | S1 | `teardownTile` `:100` |
| redeploy attached app | n/a (refused) | `Enqueue(app,"volume")` `:106`, ign |

Class A: API deletes an attached volume and redeploys the app without the mount; web refuses.

### S3. Delete managed instance

| step | web `db/handler.go:445` | api `databases.go:409` |
|---|---|---|
| gate | `requireFileUnowned` `:453`; `RequireConfirm` `:456`; any provision → refuse `:462` | `rejectManaged` `:418`; provisions → 409 unless `force=true` `:424` |
| stop service | `dbs.Remove` → `RemoveService` `managedtiles.go:571` | `envnet.TearDown` → `RemoveService` + label sweep |
| release shared overlay | `netpool.ReleaseDB` `managedtiles.go:576` | **missing** |
| `px.RemoveApp` | **missing** (s3 instances can hold domains, `db/handler.go:575`) | `helpers.go:132` |
| `DeleteTile` | `:467` | `:134` |

Class B both directions. API force-delete also leaves slice rows/data without dropping them (comment `:420-423` acknowledges).

### S4. Create managed instance

| step | web `project/handler.go:1650` | api `databases.go:207` | cfg `apply.go:1190` | orgconf `:960` | envops clone `:80` |
|---|---|---|---|---|---|
| gate | `!ConfigManaged` → stage `:1710`; else `managedErr` `:1716` | `managedGuard` `:213` | plan | file | — |
| `NewDB` | `:1703` | `:241` | `:1197` | `:967` | `:81` |
| `CreateTile` | unreachable `:1719` | `:245` | `:1210` | `:970` | `:100` |
| `ReplaceTileVars` / `syncBindings` / `syncDomains` | — | — | `:1213,1217,1223` | — | — |
| `PublishConnection` | unreachable `:1722` | `:247` | `:1220` | `:973` | `:104` |
| `Deploy` + status | — | `:249-259` inline, request ctx | `:1227-1231` | `:975-983` | **none** (clone stays idle) |

Class A: web can only stage; API creates and deploys synchronously and never stages, so a UI-managed stack gets an unstaged live DB from the CLI. Dead code `project/handler.go:1716-1729`.

### S5. Managed settings save → redeploy

web `db/handler.go:475` (`requireFileUnowned`; always redeploy if running, no changed-field check) · api `databases.go:103` (`rejectManaged`; redeploy only on container-borne field change `:120-160`) · cfg `apply.go:1461-1473` (`needsDBRedeploy(fields)`; `error` status also redeploys) · orgconf `:941`. Same order everywhere (row then Deploy). Web SetScope `db/handler.go:527` uses `RequireUnmanaged`; API scope via `applyScope` `:165` under `rejectManaged`. Not drift in order; three field-change predicates.

### S6. Stop / restart tile

Identical order web `app/handler.go:329-391` and api `lifecycle.go:30-67` (scale/restart, then status). Differences: web logs a failed status write and fires `statusChanged`; api returns it and fires nothing (notifier shard). Managed instances: web only (`db/handler.go:265-300`, `dbs.Stop/Start`); api has no stop/start/deploy for a managed instance (grep `func (a *API) (deployDB|startDB|stopDB)` empty).

### S7. Deploy / rollback / cancel

web `deployment/handler.go:73-137` vs api `apps.go:480`, `lifecycle.go:103-125`. Same engine calls. Class A: web refuses `Kind=="cron"` (`:78`), API does not (`apps.go:480-491`); engine only refuses volumes (`engine.go:212`); cfg deliberately enqueues crons (`apply.go:1237`). Rollback has no `UpperEnv` gate on either surface while deploy has.

### S8. Provision / attach slice → wire env → redeploy

| step | web `app/handler.go:866,900,930` | api `databases.go:270,320,370` |
|---|---|---|
| gate | `rejectManaged` `:906,:936` | none (`:275-283`, `:320-330`) |
| `Provision`/`AttachExisting` | `:917,:951` | `:310,:343` |
| config-managed | `!uiManaged` → text only `:874` | `ConfigManaged` → return `:378` |
| `AutoInjectAll` engines | **not handled**: single `Ref` var `:868` | whole output set `:381-389` |
| `UpdateTile` → `Enqueue("provision")` | `:887,:890` | `:384-388`, `:403-405` |

Class A on gate (API skips `rejectManaged`), class A on s3 wiring (web injects one url var for an engine whose slices need endpoint+bucket+keys, `managedtiles.go:181`).

### S9. Env teardown

One implementation `envops.Teardown` `envops.go:511`. Callers differ in gate only: web `project/handler.go:392` (`ConfigManaged`→409, `RequireConfirm`), api `envs.go:80` (`rejectManaged`, `envRunning` unless `force`), prhook `:698` (none). Reset: web `:432` re-plans (`RunAll` `:437`), api `:107` does not. Class B on reset (plan shard).

### S10. Variables → ClearWaiting → replan

Every surface calls `ClearWaiting*` after the upsert (D22-D29). Replan after: web stack/env `RunAll`/`replanAsync` (`project/handler.go:933,2303,2603`), web org config `orgcfg.Plan` (`org/config.go:184`); web org settings `:442` none; api `variables.go:176` none. Class B (plan shard owns it).

### S11. Promote with plan

`EnqueueApply` `job.go:112` dedupes on `cp.ID` and is documented as the only entry. web `releases.go:322` and api `releases.go:158` enqueue raw with dedupe `stack.ID`, and web hard-codes `Force:true` while api forwards `in.Force`. Class A. Promote without plan runs `applier.Promote` inline on the request context (`releases.go:335`, api `:146`), which `job.go:52-57` says is the failure mode the queue exists to avoid.

### S12. Cron lifecycle

`LoadSchedules` after every row change except web tile delete (S1). Run-now: web any kind (J1), api cron/function only (J2). Toggle: web any kind (J5), api cron only (J6). Class A ×2. `LoadSchedules` error: returned only at api create (`apps.go:162`), ignored at the other 14 sites.

### S13. Storage probe / storage delete

Same sequence in web `server/storage.go` and api `storage.go`, `lifecycle.go:305`; "still attached" computed by `attachedConsumers` (web) vs inline `ParseAttachment` scan (api). Duplicate rule, not drift on sequence.

## 3. Sketched service methods

**`DeployService`** (wraps `infra/deploy.Engine`, `envnet.UpperEnv`):
- `Deploy(ctx, tile, trigger) (id, error)` — owns the cron/volume/upper-env rule once (S7).
- `Rollback(ctx, tile, tag)`, `Cancel(ctx, deploymentID)`, `Park(ctx, tile, sha)`.
- `RedeployIfRunning(ctx, tile, trigger)` — the `Status=="running"` check from D13/D16 and the volume-target loop from D9/D10/D12.
- `ClearWaiting(ctx, ownerKind, ownerID, name)` — one switch instead of D22-D29.
- Callers: TileService (create/patch/delete/variables), VolumeService, StorageService, ProvisionService, ReleaseService, WebhookService, ConfigApplier.

**`RuntimeService`** (wraps `cluster` + `envnet.ServiceFor`):
- `Stop(ctx, tile)`, `Restart(ctx, tile)` — scale/restart + status write + notify (S6).
- `Teardown(ctx, tile)` — `envnet.TearDown` + proxy route + provision detach, then row (S1), one copy for web/api/cfg.
- Callers: TileService, EnvService (via envops), ConfigApplier.

**`ManagedInstanceService`** (wraps `managedtiles.Service`):
- `Create(ctx, stack, env, spec) (*Tile, error)` — NewDB → row → PublishConnection → Deploy → status (S4).
- `SaveSettings(ctx, tile, fields)` — one `needsRedeploy` predicate (S5).
- `Deploy/Start/Stop(ctx, tile)` — background with own ctx + status + notify (M6-M8).
- `Delete(ctx, tile, force)` — `dbs.Remove` + `netpool.ReleaseDB` + proxy route + row (S3).
- Callers: TileService, EnvService, ConfigApplier, OrgConfApplier.

**`ProvisionService`**:
- `Provision/Attach(ctx, instance, consumer, name, public, envVar)` — gate, wire (`AutoInjectAll` aware), redeploy (S8).
- `Detach`, `Drop`, `Fork`, `SetPublic` (persist `Public`, cf. M31).
- Callers: TileService, ManagedInstanceService, EnvService, ConfigApplier.

**`JobService`** (wraps `infra/jobs`):
- `RunNow(ctx, tile, trigger, actor)` — kind check once (J1/J2).
- `Toggle(ctx, tile)` — kind check once (J5/J6).
- `Reload(ctx)` — called by TileService after every cron row change (J7-J15).
- Callers: TileService, EnvService, ConfigApplier.

**`ApplyQueue`** (wraps `workqueue` + `stackconf.EnqueueApply`):
- `EnqueueApply(ctx, stack, plan, force, promote *Promote)` — single dedupe key (S11).
- Callers: PlanService, ReleaseService, WebhookService.

**`NodeService`** (wraps `runtime` node ops + `placement`): `SetAvailability`, `Remove`, `SetGroup`, `JoinScript`; `RawVolumeCreate/Delete` (C1/C2, no row — see calibration volumes C). Callers: ServerHandler only today.

## 4. Operations a lens-1 shard likely missed

- Web `RunNow` `app/handler.go:193` runs any tile kind; API `apps.go:501` restricts to cron/function. (tiles)
- Web `ToggleCron` `app/handler.go:397` pauses any tile kind; API `lifecycle.go:79` cron only. (tiles)
- Web `Deploy` `deployment/handler.go:78` refuses crons; API `apps.go:480` and cfg `apply.go:1237` build them. (stacks+deploys)
- API `deleteApp` `apps.go:463` never detaches consumer provisions; web `:1636` and cfg `:1562` do. (tiles / databases)
- Web `Delete` `app/handler.go:1612` never `LoadSchedules` for a deleted cron. (tiles)
- API `deleteVolume` `volumes.go:88` deletes an attached volume; web `:1622` refuses. (volumes)
- API `deleteDB` `databases.go:437` uses `teardownTile`, so `netpool.ReleaseDB` is never called; web `dbs.Remove` never `px.RemoveApp`. (databases / domains)
- Web `CreateDB` `project/handler.go:1716-1729` is unreachable; web can only stage a managed create while API `databases.go:207` creates+deploys unstaged. (databases)
- API `provisionDB`/`attachProvision` `databases.go:270,320` have no `rejectManaged`; web `:906,:936` do. (databases)
- Web `wireProvision` `app/handler.go:866` ignores `AutoInjectAll` (s3); API `:381` honours it. (databases / envs+variables)
- API `setProvisionPublic` `lifecycle.go:297` sets `p.Public` in memory only; whether `SetBucketPublic` persists it: not checked. (databases)
- Promote-with-plan `releases.go:322` (web, Force hard-coded true) and api `releases.go:158` (Force from body) bypass `EnqueueApply` and dedupe on stack id. (stacks+deploys+releases)
- Web `ResetEnvironment` `project/handler.go:437` re-plans; API `resetEnv` `envs.go:91` does not. (envs)
- Web `Stop`/`Restart` `app/handler.go:329-391` fire `statusChanged`; API `lifecycle.go:30-67` never notifies. (settings+notifications)
- Web org variable save `org/settings.go:442` and API `variables.go:176` clear waiting but do not replan; web stack/env saves do. (envs+variables)
- Web `server/handler.go:269,303` create/delete raw docker volumes with no row and no in-use check. (volumes+storage; extends calibration C)
- `service/admin.go:16` imports `docker/api/types/swarm` above `infra/`. (class D)
