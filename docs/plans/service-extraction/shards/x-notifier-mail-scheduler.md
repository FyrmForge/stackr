# Shard x: notifier + mail + scheduler reload (lens 2, call sites)

Packages: `internal/stackrd/handlers/notify/notify.go` (Notifier: `Push` writes rows + ws room `notifications`; `Project`/`Org`/`Containers`/`Server`/`Flows` are data-free ws nudges, `notify.go:102-157`), `internal/stackrd/infra/mail/mail.go` (`Mailer.Send`, `mail.go:58`), scheduler reload = `infra/jobs/jobs.go:296 LoadSchedules` (cron tiles, full drop+re-add) and `infra/backup/backup.go:83 LoadSchedules` (backup rows, same). `envops.Teardown/CloneTiles/TeardownStack` and `stackconf.Applier.CopyTile` do **not** reload schedules themselves; every caller must.

## 1. Call-site table

Surface: web / api / config / infra(bg). "commit" = store write the effect follows. Err = how a failing effect is handled.

### 1a. Notifier `Push` (persistent notification, kind gated per user)

| caller | surface | operation | fired | rule | before/after commit | err |
|---|---|---|---|---|---|---|
| `infra/deploy/engine.go:533` | infra | deploy finished, status error | `Push(KindDeployFailed, "Deploy failed: "+name, d.Error, /deployments/id)` | `d.Status=="error"` | after `UpdateDeployment` `:525` | Push swallows (`notify.go:125`) |
| `infra/deploy/engine.go:536` | infra | deploy finished, status done | `Push(KindDeployDone, "Deployed "+name, "", /deployments/id)` | `d.Status=="done"` | after `:525` | swallowed |
| `infra/cigate/cigate.go:117` | infra | CI-gated deploy failed | `Push(KindDeployFailed, "Deploy blocked: "+name, msg, /deployments/id)` | `CIFailing` verdict `:95` | after `Engine.FailWaiting` `:112` | swallowed |
| `infra/jobs/jobs.go:421` | infra | cron run ended in error | `Push(KindCronFailed, "Cron service failed: "+name, tail(out,300), /apps/id)` | only ok→error transition `:420` | after `RecordTileRun`/`finishRun` `:415-416` | swallowed |
| `infra/imagewatch/watcher.go:142` | infra | new image digest, policy auto | `Push(KindImageUpdate, "New image version: "+name, "auto-updating to "+dg, /apps/id)` | `UpdatePolicy=="auto"` | after `SetTileLatestDigest` `:131`, after `autoUpdate` `:141` | swallowed |
| `infra/imagewatch/watcher.go:145` | infra | new image digest, policy notify | `Push(KindImageUpdate, "New image version: "+name, ref+" moved to "+dg, /apps/id)` | default branch | after `:131` | swallowed |
| `infra/imagewatch/watcher.go:204` | infra | image check failed | `Push(KindImageUpdate, "Image check failed: "+name, msg, /apps/id)` | only when msg changed `:200` | after `w.record` `:203` | swallowed |

No handler (web/api) or config caller invokes `Push`. `CreateNotification` is called only from `notify.go:116`.

**Class A (payload):** `KindDeployFailed` is fired from two places with different title prefixes (`engine.go:534` "Deploy failed: " vs `cigate.go:117` "Deploy blocked: ") and different body sources (`d.Error` vs CI detail). Same kind, same link shape. `KindImageUpdate` carries three different bodies (`watcher.go:143,146,205`); the third is a check failure, not an update, under the update kind.

### 1b. Notifier ws nudges (`Project` / `Org` / `Containers` / `Server` / `Flows`)

| caller | surface | operation | fired | rule | before/after commit | err |
|---|---|---|---|---|---|---|
| `handlers/api/v1/forward.go:118` | api | port-forward presence open/close | `Project(stackID)` | `a.notify!=nil && stackID!=""` `:115`; nil-guarded (only site that guards) | no store write | n/a |
| `handlers/web/handler/app/handler.go:341` (`Stop` `:330`) | web | stop tile | `statusChanged`→`Project(a.StackID)`+`Containers()` `:392-395` | none | after `UpdateTileStatus` `:338` (status err only logged) | fire-and-forget |
| `app/handler.go:364,370` (`Restart` `:352`) | web | restart tile | same | fires on both fail and ok path | after `UpdateTileStatus` `:361/:367` | fire-and-forget |
| `app/handler.go:411` (`ToggleCron` `:398`) | web | pause/resume cron | same | none | after `UpdateTileStatus` `:407` and `LoadSchedules` `:410` | fire-and-forget |
| `handlers/web/handler/db/handler.go:255,261` (`Deploy` `:243`) | web | deploy managed db | `statusChanged`→`Project`+`Containers` `:305-308` | inside goroutine, both paths | after `UpdateTileStatus` `:252/:258` | fire-and-forget |
| `db/handler.go:279` (`Stop` `:268`) | web | stop managed db | same | none | after `:276` | fire-and-forget |
| `db/handler.go:297` (`Start` `:286`) | web | start managed db | same | none | after `:294` | fire-and-forget |
| `db/handler.go:439` (`SetProvisionPublic` `:398`) | web | bucket public/private | `Project(d.StackID)` | only on full success | after `SetBucketPublic` loop `:429` (no store row) | fire-and-forget |
| `handlers/web/handler/container/handler.go:206,218,239` | web | container start/stop/remove | `Containers()` | none | after cluster call, no store | fire-and-forget |
| `handlers/web/handler/project/handler.go:1122` (`SaveNodePosition` `:1083`) | web | env canvas layout | `Project(env.StackID)` | `RequireStackAccess` `:1104` | after `SaveNodePositions` `:1119` | fire-and-forget |
| `project/handler.go:1139` (`ResetNodePositions` `:1128`) | web | reset env layout | `Project` | `RequireStackAccess` | after `DeleteNodePositions` `:1136` | fire-and-forget |
| `project/handler.go:2536` (`replanAsync` `:2525`) | web | stack/env var save+delete (`SaveStackVar :2285`, `DeleteStackVar :2313`, `SaveEnvVar :2578`, `DeleteEnvVar :2613`) | `Project(stack.ID)` | `p.ConfigManaged()` `:2526`; goroutine, `context.Background()` | after `Planner.RunAll` `:2532` | plan err discarded `:2532` |
| `handlers/web/handler/project/stackgraph.go:95,109,121,135,147,161,173,187,199,226` | web | stack/env canvas positions, annotations, groups (save/delete/reset) | `Project(p.ID / stackID)` | `loadStack` / `loadEnvForAnnotation` `:205` | after store write | fire-and-forget |
| `handlers/web/handler/org/graph.go:117,129,162,174,309,323` | web | org canvas annotations, groups, positions, reset | `Org(o.ID)` | `loadOrg` | after store write | fire-and-forget |
| `handlers/web/handler/prhook/handler.go:303` (`planConfigs` `:250`) | web (webhook) | env-scoped config plan after push | `Project(s.ID)` | per env, after `maybeAutoApply` `:302` | after `pl.RunEnv` `:296` | plan err logged, continue |
| `prhook/handler.go:319` | web (webhook) | stack-scoped config plan after push | `Project(s.ID)` | `err==nil` `:311` | after `pl.Run` `:306` + `maybeAutoApply` loop | plan err logged |
| `infra/deploy/engine.go:529-530` | infra | deploy finished | `Project(app.StackID)`+`Containers()` | always | after `UpdateDeployment` `:525` | fire-and-forget |
| `infra/jobs/jobs.go:348` (`StartApp` `:332`) | infra (called by web RunNow + api `runApp`, schedule tick) | run queued | `Project` | after `work.Enqueue` `:344` | after `startRun` `:343` | fire-and-forget |
| `infra/jobs/jobs.go:363,369,377,417` (`runApp` `:356`) | infra | run skipped / no image / finished | `Project` | each terminal path | after `finishRun` | fire-and-forget |
| `infra/metrics/reconcile.go:112` | infra | status reconcile changed tiles | `Project(stackID)` per changed stack | `UpdateTileStatus` ok `:107` | after | fire-and-forget |
| `infra/metrics/metrics.go:136` | infra | flow sample tick | `Flows()` | `ListAll` ok | no store | n/a |
| `infra/metrics/metrics.go:179` | infra | host sample | `Server()` | always | after `PruneMetrics` `:178` | n/a |

### 1c. Scheduler reload `jobs.LoadSchedules`

| caller | surface | operation | rule | before/after commit | err |
|---|---|---|---|---|---|
| `handlers/api/v1/apps.go:162` (`createApp` `:60`) | api | create tile | `kind=="cron" && a.jobs!=nil` | after `CreateTile` `:157` | **returned** as 500 `:163` (only site that returns it) |
| `handlers/api/v1/apps.go:458` (`patchApp` `:177`) | api | update tile settings | `t.Kind=="cron" && a.jobs!=nil` | after `UpdateTile` `:453` | discarded |
| `handlers/api/v1/apps.go:475` (`deleteApp` `:463`) | api | delete tile | `t.Kind=="cron" && a.jobs!=nil` | after `teardownTile`→`DeleteTile` `helpers.go:134` | discarded |
| `handlers/api/v1/lifecycle.go:93` (`toggleCron` `:76`) | api | pause/resume cron | `a.jobs!=nil` | after `UpdateTileStatus` `:89` | discarded |
| `handlers/api/v1/envs.go:84` (`deleteEnv` `:58`) | api | delete env | `a.jobs!=nil`, unconditional on kinds | after `envOps().Teardown` `:80` | discarded |
| `handlers/api/v1/envs.go:111` (`resetEnv` `:91`) | api | reset env | `a.jobs!=nil` | after `Teardown` `:107` | discarded |
| `handlers/api/v1/envs.go:151` (`copyEnv` `:118`) | api | copy env | `a.jobs!=nil` | after `CloneTiles` `:147` | discarded |
| `handlers/web/handler/app/handler.go:410` (`ToggleCron` `:398`) | web | pause/resume cron | none (no nil guard) | after `UpdateTileStatus` `:407` | discarded |
| `app/handler.go:1592` (`SaveSettings` `:1428`) | web | update tile settings, direct path | `a.Kind=="cron"`; staged path `:1581` skips (apply covers) | after `UpdateTile` `:1588` | discarded |
| `handlers/web/handler/project/handler.go:353` (`CreateEnvironment` `:305`) | web | create env | only when `BaseEnvID!=""` (clone) | after `CloneTiles` `:350` | discarded |
| `project/handler.go:392` (`DeleteEnvironment` `:363`) | web | delete env | none | after `Teardown` `:389` | discarded |
| `project/handler.go:432` (`ResetEnvironment` `:409`) | web | reset env | none | after `Teardown` `:429` | discarded |
| `project/handler.go:611` (`CreateTile` `:471`) | web | create tile, direct path | `kind=="cron"`; staged path `:592` skips | after `CreateTile` `:607` | discarded |
| `handlers/web/handler/project/envcompare.go:146` (`CopyEnv` `:118`) | web | copy one tile across envs | `h.jobs!=nil`, unconditional on kind | after `applier.CopyTile` `:142` | discarded |
| `handlers/web/handler/prhook/handler.go:534` (`openPR` `:462`) | web (webhook) | PR env open | none | after `ApplyResolved` `:526` (which already reloads when `cronTouched`, `apply.go:1099`) | discarded |
| `prhook/handler.go:702` (`closePR` `:691`) | web (webhook) | PR env close | none | after `ops.Teardown` `:699` | discarded |
| `config/stackconf/apply.go:1100` (`execute` `:853`) | config | apply plan | `cronTouched` set at `:1026,:1042,:1054,:1096` (tile create/update/delete, env delete) `&& a.Jobs!=nil` | after every tile/env write | `warn(...)` logged, apply continues |
| `cmd/stackrd/main.go:427` | boot | startup | -- | -- | logged |

### 1d. Scheduler reload `backup.LoadSchedules`

| caller | surface | operation | rule | before/after commit | err |
|---|---|---|---|---|---|
| `handlers/api/v1/backups.go:208` (`createBackup` `:154`) via `reloadBackupSchedules` `:402` | api | create backup | `a.backups!=nil` | after store write | discarded |
| `backups.go:284` (`patchBackup` `:230`) | api | update backup | same | after | discarded |
| `backups.go:297` (`deleteBackup` `:288`) | api | delete backup | same | after | discarded |
| `handlers/web/handler/backups/handler.go:236` (`Create` `:189`) via `reload` `:327` | web | create tile backup | `h.svc!=nil` | after | discarded |
| `backups/handler.go:273` (`Save` `:242`) | web | update tile backup | same | after | discarded |
| `backups/handler.go:288` (`Delete` `:280`) | web | delete tile backup | same | after | discarded |
| `handlers/web/handler/settings/handler.go:254` (`SavePanelBackup` `:216`) | web | create/update panel (stackr) backup | `h.backups!=nil` | after `CreateBackup/UpdateBackup` `:246-248` | discarded |
| `settings/handler.go:286` (`DeletePanelBackup` `:276`) | web | delete panel backup | same | after `DeleteBackup` `:282` | discarded |
| `config/stackconf/apply.go:988` (`execute`) | config | apply plan, backups section | `applyBackups` ok `&& a.Backups!=nil` | after `applyBackups` `:983` | `warn` logged |
| `cmd/stackrd/main.go:448` | boot | startup | -- | -- | logged |

### 1e. Mail `Mailer.Send`

| caller | surface | operation | fired | rule | before/after commit | err |
|---|---|---|---|---|---|---|
| `handlers/web/handler/org/members.go:290` (`mailInvite` `:285`) called from `newInvite :277` (used by `AddMember :249`, `CreateInvite :400`, `ReinviteMember :465`) and `ResendInvite :447` | web | invite member / create invite / resend / reinvite | invite email, subject `"You have been invited to "+o.Name+" on stackr"`, text+html inline `:291-295` | `inv.Email!="" && h.mail.Enabled()` `:286` | after `CreateInvite` `:274` | logged, `inv.MailFailed=true` `:297-298`, not persisted (field set on the in-memory row only; not checked whether a later `UpdateInvite` stores it) |
| `handlers/api/v1/members.go:210` (`newInvite`) called from `addMember :87`, `createInvite :184` | api | invite member / create invite | **nothing** | -- | -- | -- |

The API `*API` struct has no `Mailer` field (`v1.go:57`). Only the web org handler receives one (`org/handler.go:45`).

## 2. Coverage matrix (operation × surface)

Y = fires, N = does not fire although the operation exists on that surface, -- = operation absent on surface, cfg = config apply path.

| operation | effect | web | api | config | class |
|---|---|---|---|---|---|
| stop tile | `Project`+`Containers` | Y `app/handler.go:341` | **N** `lifecycle.go:30-44` | -- | B |
| restart tile | `Project`+`Containers` | Y `app/handler.go:364,370` | **N** `lifecycle.go:48-71` | -- | B |
| pause/resume cron | `Project`+`Containers` | Y `app/handler.go:411` | **N** `lifecycle.go:76-97` | -- | B |
| pause/resume cron | `jobs.LoadSchedules` | Y `:410` | Y `:93` | -- | |
| create tile (cron) | `jobs.LoadSchedules` | Y `project/handler.go:611`; staged→cfg | Y `apps.go:162` (returns err) | Y `apply.go:1100` | A (err handling) |
| update tile settings (cron) | `jobs.LoadSchedules` | Y `app/handler.go:1592`; staged→cfg | Y `apps.go:458` | Y `apply.go:1100` | |
| delete tile (cron) | `jobs.LoadSchedules` | **N** `app/handler.go:1606-1645` (direct path `:1632-1642` deletes row, no reload); staged→cfg Y | Y `apps.go:475` | Y `apply.go:1096-1100` | B |
| delete env | `jobs.LoadSchedules` | Y `project/handler.go:392` | Y `envs.go:84` | Y `apply.go:1096` | |
| reset env | `jobs.LoadSchedules` | Y `:432` | Y `envs.go:111` | -- | |
| copy env / create env from base | `jobs.LoadSchedules` | Y `:353` | Y `envs.go:151` | -- | |
| copy one tile across envs | `jobs.LoadSchedules` | Y `envcompare.go:146` | -- | -- | |
| delete stack | `jobs.LoadSchedules` | **N** `project/handler.go:445-468` (`TeardownStack :460` tears down every env, `envops.go:598`) | **N** `stacks.go:52-67` | -- | B (both surfaces; stale cron entries survive until next reload, `jobs.go:336-342` then logs "not found") |
| PR env open/close | `jobs.LoadSchedules` | Y `prhook:534,702` | -- | -- | |
| create/update/delete tile backup | `backup.LoadSchedules` | Y `backups/handler.go:236,273,288` | Y `backups.go:208,284,297` | Y `apply.go:988` | |
| create/update/delete panel backup | `backup.LoadSchedules` | Y `settings/handler.go:254,286` | Y (same `createBackup`/`patchBackup`/`deleteBackup`, `BackupStackr` branch `backups.go:124,188,258`) | -- | |
| deploy managed db | `Project`+`Containers` | Y `db/handler.go:255,261` | **N** `databases.go:249-259` (`createDB` deploys inline, no nudge) | not checked (apply path deploys via managedtiles; no notifier field on Applier) | B |
| start/stop managed db | `Project`+`Containers` | Y `db/handler.go:279,297` | -- (no route) | -- | |
| set provision/bucket public | `Project` | Y `db/handler.go:439` | **N** `lifecycle.go:274-300` | -- | B |
| save/delete stack or env var (config-managed) | replan + `Project` | Y `project/handler.go:2303,2323,2603,2630` → `:2532-2536` | **N** `variables.go:245 putStackVars`, `:296 putEnvVars` (no `RunAll`, no notify) | -- | B (plan banner stale after API var write) |
| plan now / approve / reject plan | `Project` | **N** `project/handler.go:748,780,808` (only webhook path `prhook:303,319` and var path nudge) | **N** `config.go:105,169,189` | -- | B (both) — canvas banner refresh only via webhook/var routes |
| staging apply | `Project` | **N** `staging.go:50` (not checked whether Applier nudges; Applier has no notifier field, `apply.go` never imports notify) | -- | -- | flag |
| canvas positions/annotations/groups (stack, env, org) | `Project`/`Org` | Y `stackgraph.go`, `org/graph.go`, `project/handler.go:1122,1139` | -- | -- | |
| container start/stop/remove | `Containers` | Y `container/handler.go:206,218,239` | -- | -- | |
| port-forward presence | `Project` | -- | Y `forward.go:118` | -- | |
| invite member / create invite | mail | Y `members.go:290` | **N** `api/v1/members.go:87,184` | -- | B |
| resend / reinvite | mail | Y `members.go:447,465` | -- (no route) | -- | |
| notification prefs read/save | -- | Y `account/handler.go:265,280` | -- | -- | web-only surface |
| notification center page/mark-read/clear/badge | -- | Y `notification/handler.go:28,41,49,57` | -- | -- | web-only surface |
| deploy finished / cron failed / image update | `Push` | infra only (`engine.go:533,536`, `cigate.go:117`, `jobs.go:421`, `watcher.go:142,145,204`) | | | uniform |

## 3. Sketched service methods

`notifier` service (wraps `notify.Notifier`; the ws-nudge and Push API stays, but handlers stop calling it):

- `TileStatusChanged(ctx, tile)` → `Project(tile.StackID)` + `Containers()`. Callers: tile service (stop/restart/toggle-cron/delete), managed-db service (deploy/start/stop), deploy engine, jobs runner, metrics reconcile. Replaces `app/handler.go:392`, `db/handler.go:305`, `engine.go:529-530`.
- `StackChanged(ctx, stackID)` → `Project`. Callers: plan service (after RunAll/RunEnv/approve/reject), staging service (after apply), variables service (after replan), provisions service (bucket public), canvas service (positions/annotations/groups), forward service.
- `OrgChanged(ctx, orgID)` → `Org`. Callers: canvas service (org scope).
- `ContainersChanged()` → `Containers`. Callers: container service.
- `DeployFinished(ctx, tile, deployment)` → `Push(KindDeployFailed|KindDeployDone, ...)` with one title/body builder. Callers: deploy engine, cigate (collapses the `engine.go:534` vs `cigate.go:117` payload split).
- `CronRunFailed(ctx, tile, out)`, `ImageUpdated(ctx, tile, digest, auto bool)`, `ImageCheckFailed(ctx, tile, msg)` → `Push`. Callers: jobs runner, imagewatch.
- `Server()`, `Flows()` unchanged, metrics sampler only.

`scheduler` service (owns both cron tables; hides which one):

- `ReloadCron(ctx) error` → `jobs.LoadSchedules`. Callers: tile service (create/update/delete when kind==cron, toggle), env service (delete/reset/copy/clone, `TeardownStack`), stack service (delete), PR-env service (open/close), config applier (`cronTouched`), copy-tile. One nil-guard, one error policy (today: one caller returns 500 `apps.go:163`, the rest discard, config warns).
- `ReloadBackups(ctx) error` → `backup.LoadSchedules`. Callers: backup service (create/update/delete, tile and panel kinds), config applier.
- Alternative worth recording, not deciding: `envops.Teardown/CloneTiles/TeardownStack` and `Applier.CopyTile` could reload internally, which would remove nine of the eighteen call sites and close both class-B rows above (`stack delete`, web `cron tile delete`).

`mail` service (wraps `mail.Mailer`):

- `SendInvite(ctx, org, invite, link) (failed bool)` with the template from `members.go:291-295`. Callers: members service (invite/create/resend/reinvite) from **both** web and API. API today has no mailer at all (`v1.go:57`).
- Link building (`inviteURL(c, id)` `members.go:289`, web-only, request-derived) needs a base URL source that is not `echo.Context` before the API can call it. Not checked where the root domain lives.

## 4. Operations a lens-1 shard likely missed (one line each)

- tiles: API `stopApp`/`restartApp`/`toggleCron` never nudge canvases (`lifecycle.go:30,48,76`); web twins do (`app/handler.go:341,364,370,411`). Class B.
- tiles: web direct-path cron tile delete does not reload the cron scheduler (`app/handler.go:1632-1642`); API does (`apps.go:474-476`). Class B. Deleted cron keeps ticking until any other reload; tick then fails at `jobs.go:340-342`.
- tiles: API `createApp` returns 500 when the schedule reload fails after the row is committed (`apps.go:162-164`); every other caller discards. Class A on error contract.
- stacks+deploys: stack delete on both surfaces tears down every env without a cron reload (`project/handler.go:460`, `stacks.go:64`, `envops.go:598`). Class B, both surfaces.
- databases: API `createDB` deploys inline with no `Project`/`Containers` nudge (`databases.go:249-259`); web `Deploy` fires (`db/handler.go:255,261`). API `setProvisionPublic` no nudge (`lifecycle.go:274`); web fires (`db/handler.go:439`). Class B.
- envs+variables: API `putStackVars`/`putEnvVars` (`variables.go:245,296`) neither replan a config-managed stack nor nudge; web does both via `replanAsync` (`project/handler.go:2525`). Class B.
- stacks+deploys (plans): web `PlanNow`/`ApprovePlan`/`RejectPlan` (`project/handler.go:748,780,808`) and API `approvePlan`/`rejectPlan` (`config.go:169,189`) do not nudge canvases; only the webhook planner does (`prhook:303,319`). Both surfaces, flag.
- orgs+members: API invite/add-member never mails (`api/v1/members.go:87,184,210`); web does (`org/members.go:277,290`). `*API` has no mailer dependency (`v1.go:57`). Class B.
- orgs+members: `inv.MailFailed` is set on the in-memory row after `CreateInvite` (`members.go:274,298`); whether it is ever persisted is not checked.
- settings+notifications: notification prefs (`account/handler.go:265,280`) and notification center (`notification/handler.go:28-57`) are web-only surfaces with no API rows; the store side (`store/repo/sqlite/notifications.go`) has no non-web consumer.
- deploy orchestration: `KindDeployFailed` fired with two payload shapes (`engine.go:533-534` vs `cigate.go:117`). Class A.
- registry+github: imagewatch reuses `KindImageUpdate` for check failures (`watcher.go:204`), so a user who opts out of "New image version" also loses "Image check failed". Flag.
