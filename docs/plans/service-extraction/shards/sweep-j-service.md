# Sweep J — service layer and store

Scope note: the brief said `store/repo` + `store/repo/sqlite` is 19 files. That
is the `sqlite/` count only; `repo/` holds 5 more (`giturl.go`, `models.go`,
`repo.go`, `slug.go`, `user.go`). All 24 were read. Nothing in this shard
carries a `// Code generated` header, so nothing was skipped as generated.
`store/db/dumpschema/main.go` is a hand-written dev tool, not generated output,
and was read.

## Files read

`internal/stackrd/service/` (40 files, 9 596 lines)

| File | Lines | File | Lines |
|---|---|---|---|
| access.go | 353 | org.go | 195 |
| admin.go | 213 | plan.go | 209 |
| apikey.go | 98 | prenv.go | 100 |
| audit.go | 33 | registry.go | 361 |
| auth.go | 192 | release.go | 154 |
| backupdestination.go | 254 | revoke.go | 230 |
| backupschedule.go | 323 | rows.go | 169 |
| connector.go | 102 | settings.go | 156 |
| container.go | 75 | slice.go | 385 |
| deploy.go | 232 | stack.go | 327 |
| domain.go | 357 | storage.go | 268 |
| domain_auto.go | 142 | tenancy.go | 339 |
| domainresource.go | 361 | tilediff.go | 187 |
| environment.go | 426 | tile.go | 578 |
| gate.go | 89 | tilelifecycle.go | 327 |
| graph.go | 99 | tiletelemetry.go | 159 |
| imagewatch.go | 302 | tilevalidate.go | 294 |
| managedinstance.go | 506 | variable.go | 367 |
| member.go | 247 | workitem.go | 67 |
| node.go | 275 | notification.go | 45 |

`internal/stackrd/service/` subpackages (5 files, 893 lines):
`mail/mail.go` 77, `notify/notify.go` 189, `proxy/proxy.go` 459,
`scheduler/scheduler.go` 72, `svcerr/svcerr.go` 96.

`internal/stackrd/store/repo/` (5 files, 1 736 lines):
`giturl.go` 59, `models.go` 1 262, `repo.go` 355, `slug.go` 33, `user.go` 27.

`internal/stackrd/store/repo/sqlite/` (19 files, 2 384 lines):
`audit.go` 47, `backups.go` 133, `deployments.go` 188, `infra.go` 311,
`intended.go` 32, `jobs.go` 55, `members.go` 68, `notifications.go` 47,
`provisions.go` 70, `secretlinks.go` 98, `servers.go` 95, `stacks.go` 381,
`staged.go` 47, `storage.go` 101, `store.go` 85, `tiles.go` 298, `users.go` 75,
`variables.go` 153, `workitems.go` 100.

`internal/stackrd/store/audit/audit.go` 29 ·
`internal/stackrd/store/db/db.go` 31 ·
`internal/stackrd/store/db/dumpschema/main.go` 117 ·
`internal/stackrd/store/testdb/` (7 files, 315 lines): `connectors.go` 20,
`links.go` 40, `noderows.go` 51, `registries.go` 37, `runrows.go` 50,
`testdb.go` 72, `workitems.go` 45 ·
`internal/stackrd/envcolor/envcolor.go` 136.

Total: 15 237 lines across 80 files.

## Hollow services

A method counts as hollow when its body is a single store call with no branch,
no validation and no side effect, **and** a rule about the same concept
demonstrably lives somewhere else. Thin-but-nothing-to-decide methods
(`AuditService.For`, `NotificationService.List`, the `Rows` forwarders) are not
listed — they are read shells over a table with one owner and no second opinion,
which is the shape the extraction deliberately produced.

| Service.Method | file:line | Where the rule actually lives |
|---|---|---|
| `OrgService.Delete` | service/org.go:193 | `handlers/web/handler/org/members.go:171-203` — "org with stacks cannot be deleted", "last organization cannot be deleted", "a setup draft doesn't count", plus post-delete `h.rt.RemoveBuilder`. Its own doc comment says the rule is "the panel's". One caller repo-wide (members.go:196), no API route, so misplaced rather than bypassable. |
| `GraphService.SavePositions` | service/graph.go:70 | Bare `store.SaveNodePositions`. The node-key normalisation lives in its callers and has already drifted between two of them: `project/handler.go:986` applies `slugKey` first, `project/stackgraph.go:93` does not. The other two callers (`org/graph.go:70`, `:307`) are **not inspected — shard B**; the org canvas draws orgs and stacks rather than tiles, so whether the same transform even applies there is B's to answer. Shape validation is one layer *below* the service, in `sqlite/infra.go:245-246` (`repo.ValidateNodePositions`), so the service owns neither half. |
| `GraphService.SaveAnnotation` / `SaveGroup` | service/graph.go:82, :92 | Same shape as `SavePositions`: bare store call, with both the validator and the row quota one layer below in `sqlite/infra.go:143-155` and `:179-198`. Listed for completeness — unlike `SavePositions` these have no observed caller drift, so they are a smaller case of the same missing ownership. |
| `StorageService.DeletePath` | service/storage.go:261 | Deliberately hollow ("the admin page's 'delete anyway' needs to get past a yes") but the consumer check **and** the volume cleanup are then spelled twice outside it: `server/storage.go:140-143` and `api/v1/storage.go:184-187` each do `clus.RemoveVolume(repo.StorageVolume(p.ID))` before calling it. The volume cleanup is not a confirmation, it is the delete's other half, and it has no owner. |
| `WorkItemService.*` (all six) | service/workitem.go:39-67 | Self-declared forwarder in its own doc comment. Its three *read* siblings never came with it: `GetWorkItem`, `ListWorkItemsByStatus` and `LatestWorkItem` are called straight off the store from `infra/workqueue/workqueue.go:216,250,274,337`, `infra/jobs/jobs.go:281`, `infra/backup/work.go:145`, `infra/volmove/volmove.go:192,346,355`. Half the table has an owner. |
| `BackupScheduleService.Save` / `.Remove` | service/backupschedule.go:296, :309 | Escape hatches for the panel-database backup. `Save` has only an id check; the destination-must-be-global rule for that path is duplicated at `settings/handler.go:242`, `backup.Validate` is called directly at `:260`, and the `ReloadBackups` the sibling methods do for free is hand-written at `:266-269`. `api/v1/backups.go:204-208` uses the same hatch for a **tile** schedule, where `Delete` exists and reloads — an unjustified bypass. |
| `RevokeService.Mint` / `.ByHash` / `.Claim` / `.BurnDrop` / `.Touch` | service/revoke.go:203-230 | Five bare store calls. Every rule about a link — dead-or-alive (`repo.SecretLink.Dead`, models.go:653), attempt counting against `repo.MaxLinkAttempts`, "stamp only on first access" (stated in `sqlite/secretlinks.go:88`) — lives in `config/sharelink`. The service's own comment admits it: "Moving the writes here does not fix that on its own." |
| `SettingsService.Value` / `.SetValue` | service/settings.go:149, :154 | Untyped passthrough to `settings` with no key validation and no resync. Four other services write the same table around it: `AdminService` (admin.go:99,102,166), `ImageWatchService` (imagewatch.go:75,272), `proxy.Service` (proxy.go:262,286-289,327,354-357) and the free functions `repo.LoadPRConfig`/`SavePRConfig` (models.go:828-840). |
| `MemberService.CreateInvite` | service/member.go:245 | Bare `store.CreateInvite`, bypassing `Invite`'s role whitelist, the 1-365 day expiry clamp and the already-a-member conflict at member.go:58-103. Justified in-comment for "the API's own minting path", but the expiry rule is exactly the one the type comment says the API used to get wrong. |
| `DomainResourceService.Save` | service/domainresource.go:322 | Bare `CreateDomainResource`, skipping `ValidateResourceHost`, `HostTaken` and `checkOwner`/squat. For the setup wizard, per comment. |
| `EnvironmentService.Remove` | service/environment.go:395 | Bare `DeleteEnvironment`, skipping the whole `teardown` cascade (staged_changes and node_positions, which nothing else cleans up — environment.go:220-236). |
| `TileService.Save` | service/tile.go:545 | Bare `UpdateTile`: no gate, no `DiffTiles`, no `afterWrite`. Its own comment says "A new caller almost certainly wants Update." |
| `StorageService.Save`, `OrgService.Save`, `AuthService.SaveUser`, `RegistryService.Create`/`Save`, `BackupDestinationService.Save`, `StackService.Reslug`, `VariableService.Upsert`/`Remove` | storage.go:266, org.go:185, auth.go:183, registry.go:354/359, backupdestination.go:252, stack.go:156, variable.go:334/345 | A family of documented raw-write hatches. Each is individually defensible; as a set they are the readmission door for exactly the drift the extraction closed, because none of them is distinguishable at the call site from the ruled method next to it. |

## Missing services

Derived by listing every table declared in `store/repo/models.go` (cross-checked
against the `repo.Store` interface in `store/repo/repo.go`, which is the
complete method surface) and asking which has a service that owns both sides.

| Domain concept | Tables involved | Who owns it today |
|---|---|---|
| **Volumes** (docker named volumes) | none — `tiles` rows with `kind=volume`, plus docker objects named by `repo.Tile.DockerVolume()` / `repo.StorageVolume()` (models.go:288, :731) | No `VolumeService`. Row rules are scattered inside `TileService` (`validateVolume` tilevalidate.go:136, `checkAttach` tile.go:387, `orphanVolumes` tile.go:251, the attached-guard tile.go:186). The docker side is owned by handlers outright: `server/handler.go:271-320` does name validation, node resolution and direct `h.clus.CreateVolume`/`RemoveVolume`. `repo.ValidVolumeName` (models.go:727) sits in the model because nothing above it would own it. |
| **The work queue** | `work_items` | `WorkItemService` owns the six writes and none of the three reads (see hollow table). `infra/workqueue` reads the table directly and is the de-facto owner. |
| **Panel-database backup** | `backups` with `tile_id IS NULL`, `backup_runs` | Nobody. `BackupScheduleService`'s every rule is written about a tile and explicitly bails on `t == nil`; the path goes through `Save`/`Remove` from `settings/handler.go:242-269`. `AdminService` owns the *archive* half (`WritePanelArchiveTo`, admin.go:153) and `repo.BackupStackr`/`AccessService.TenancyOf`'s `ErrServerOwned` (tenancy.go:82,142) both exist to describe a row no service owns. |
| **Sessions** | `sessions` | No service. `sqlite/store.go:40-85` implements `hamr/pkg/auth.SessionStore` directly. Framework-owned; listed for completeness, not as a gap to close. |
| **Audit events (write side)** | `audit_events` | `AuditService` reads only, by design (audit.go:11-14). The write is `store/audit.Record`, called from 13 places outside `service/` — see below. |
| **Secret links** | `secret_links` | Split. Mint/claim/burn are hollow methods on `RevokeService`, a service named for the revocation half; the rules are in `config/sharelink`. There is no `ShareLinkService`. |
| **Notifications** | `notifications` | Split by design but with no named owner: `NotificationService` reads and clears (notification.go), `notify.Notifier.Push` writes (notify/notify.go:115-148) including the `PruneNotifications` retention call at :146. Two packages, one table. |
| **Intended values** | `env_intended` | Owned by `ManagedInstanceService` (managedinstance.go:459-472). Wrong owner — see next section. |
| **Metrics** | `metrics` | Two owners: `TileTelemetryService.RecordSample`/`Prune` (tiletelemetry.go:28,33) and `NodeService.RecordSample` (node.go:243). `service/rows.go:65-71` routes the infra interface to the telemetry one, so the node one is a second door on the same table. |
| **Boot-time sweep** | `deployments`, `tiles` | `store.SweepStaleRuns` is called from `cmd/stackrd/main.go:365` with no service in between, and the policy it applies lives in SQL (see below). |

## Rules in the wrong service

1. **`env_intended` belongs to environments, not instances.**
   `ManagedInstanceService.Intended` / `SetIntended` / `ClearDeclaredIntended`
   (service/managedinstance.go:459-472). An `Intended` row is
   `(environment_id, tile_slug, key, value)` — a per-environment drift
   annotation with no managed instance in it. `EnvironmentService` is the owner
   the table names. The file header even files them under "managed resources",
   which they are not.

2. **The image watcher writes `cron_runs`.**
   `ImageWatchService.recordWatch` (service/imagewatch.go:244-253) inserts a
   `repo.CronRun` under `ImageWatchRef(tileID)` = `"watch:<id>"`.
   `TileLifecycleService` owns that table (`StartRun`/`FinishRun`/`PruneRuns`,
   tilelifecycle.go:291-303). The watch reuses cron_runs for retention "for
   free" — a deliberate trick, but it means two services write one table and
   `PruneRuns` silently governs the watch history's retention too.

3. **`NodeService.RecordSample`** (service/node.go:243) writes `metrics`, which
   `TileTelemetryService` owns. Duplicate of tiletelemetry.go:28; `Rows`
   (rows.go:66) already routes the infra interface to the telemetry service, so
   the node copy has no caller path that needs it.

4. **`DeployService.RedeployIfRunning`** (service/deploy.go:106) encodes
   *variable*-write policy — "roll a running service so it re-reads its env" —
   inside the deploy service, with the four-clause kind/status filter at :114.
   Its own comment says this was pulled out of `VariableService`; the predicate
   is fine where it is, but `VariableService.redeploy` (variable.go:293) is now
   a one-line wrapper whose doc comment carries the *reason*, so the rule and
   its justification live in two services.

5. **`ManagedInstanceService.tiles.TearDown`** (managedinstance.go:378) —
   instance teardown ends by calling `TileService.TearDown`, which itself calls
   `orphanVolumes` and the volume-target redeploy (tile.go:232-243). Those two
   are volume rules executing on behalf of a database instance that can never
   have a volume tile attached (`checkAttach` refuses `target.IsManaged()`,
   tile.go:392). Harmless today, but it is the volume rule set running under
   two services because there is no third one to own it.

6. **`PlanService.Work`** (service/plan.go:207) reads `work_items` through
   `LatestWorkItem`. That is the work queue's table; `PlanService` reaches past
   `WorkItemService` because `WorkItemService` has no read methods.

7. **`AccessService.TenancyOf`'s `KindBackup` branch** (tenancy.go:132-145)
   encodes the panel-database backup rule — "a NULL tile means server-owned,
   admin only" — inside the tenancy resolver, because no backup service owns it.
   The comment says so outright.

## Business logic in store/repo/sqlite

Everything below is a decision about the domain encoded under
`store/repo/sqlite/`, which the plan says should be SQL and nothing else.

| Where | Rule |
|---|---|
| `deployments.go:55-68` **SweepStaleRuns** | The interrupted-run policy, in three parts: queued+running deployments become `error` with `repo.InterruptedMsg` (the string the deploy engine matches on to requeue — models.go:752-756 says a drift between the two silently declines to run); tiles at `building` become `stopped`, not `error`; and `backup_runs` is deliberately *excluded*, with a comment explaining which other subsystem covers it. Three policy choices in a `[]string` of SQL, reached from `cmd/stackrd/main.go:365`. |
| `tiles.go:31-42` **CreateTile defaults** | `ScopeKind` defaults to `"env"`, `Replicas < 1` becomes 1, `EndpointProtocol` defaults to `"http"`, `UpdatePolicy` defaults to `"off"`. `TileService.applyDefaults` (tile.go:347) fills a *different* set; these four are only ever filled here, so a tile created through `Rows.SaveTile` or any non-service writer silently gets them and a caller reading its own struct back does not. |
| `tiles.go:76-88` **projectEnvVars** | Every `UpdateTile` and `CreateTile` parses the env blob and upserts `variables` rows. A cross-table projection — the tile→variables mirror — running inside the tile writer. `VariableService.syncBlob` (variable.go:236) exists solely to fight it ("the next tile write projects it straight back"). |
| `tiles.go:97-118` **ReplaceTileVars** | "The file is the whole truth": drops every non-secret variable row the blob does not declare, keeps secrets. The config-ownership rule, in the persistence layer. `VariableService.ReplaceTileVars` (variable.go:355) is a hollow wrapper over it. |
| `tiles.go:288-296` **DeleteTile** | Cascades `variables` and `resource_bindings` by hand ("a reused id would otherwise inherit the dead tile's values and grants"). A security decision. |
| `stacks.go:107-133` **DeleteOrg** | Hand-cascades `node_positions` across three owner-key shapes (org, per-stack, per-environment) because the table has no FK. Canvas-ownership knowledge inside the org delete. |
| `stacks.go:146-166` **CreateStack** | Inserts the home environment in the same transaction ("a stack without a home has nowhere to put a shared tile"). `EnvironmentService.EnsureHome` (environment.go:415) is the repair path for the same invariant, one layer up. |
| `stacks.go:264-269` + `:96-101` **SupersedePendingPlans / SupersedePendingOrgPlans** | `status IN ('pending','clean','error')` is a policy statement — which plan states count as "undecided" — with an eight-line comment arguing for it. `'applied'` and `'rejected'` are history. Nothing in `PlanService` says this; `PlanService.SupersedePending` (plan.go:167) forwards blind. |
| `stacks.go:221-224` **LatestSettledConfigPlan** | `status IN ('applied','clean')` defines "the stack as it runs". |
| `stacks.go:229-236` **CountStacksAwaitingPlan** | `status IN ('pending','error')` **and newest-only** defines "waiting on somebody". |
| `stacks.go:302-306` **ListEnvironmentsByStack** | `type != 'stack'` hides the home env from every listing, and `ORDER BY type = 'ephemeral', position, created_at` *is* the ladder. `EnvironmentService.ListForStack` (environment.go:336) documents the order as load-bearing and cannot enforce it. |
| `deployments.go:76-81` **storePath** | `"" → "/"` normalisation, with a comment saying callers disagree (config sends `""`, API sends `"/"`) and that a raw `""` would slip a second router past the uniqueness index. A domain rule placed here *because* two surfaces drifted — the same pattern the sweep is cataloguing, solved one layer too low. `DomainService.plan` (domain.go:101-103) has its own copy. |
| `infra.go:245-265` **SaveNodePositions** | Calls `repo.ValidateNodePositions` before writing, with a comment saying validation lives here "rather than in each handler". Same for `UpsertAnnotation` (`infra.go:143-155`, cap check + `ValidateAnnotation`) and `UpsertGraphGroup` (`infra.go:179-198`, cap check + `ValidateGraphGroup`). Three validators and two quota enforcements in the persistence layer; `GraphService` (graph.go) has none. |
| `infra.go:291-297` **DeleteOrgRegistryCredential** | `AND system = 0` silently refuses the system credential. A refusal expressed as a no-op row count — the caller cannot tell "refused" from "already gone". `RegistryService.RevokeCredential` (registry.go:237) has the *explaining* version; this is the second, silent one. |
| `storage.go:24-26` **CreateStorage** | `ServerID` defaults to `"local"` when neither owner is set. `StorageSpec`'s own comment (storage.go:43-46) says hard-coding `"local"` was the bug that put every CLI-created pool on the manager — and the hard-coding is still here, one layer down. |
| `storage.go:47-49` **GetStorageBySlug** | `AND server_id IS NOT NULL` encodes "org shares are not found by global slug". A namespacing rule in a `WHERE`. |
| `backups.go:46-57` **UpdateBackupDestination** | An empty `SecretKey` means "keep the stored one", implemented by a read-back. A form convention ("the form never renders the secret back") enforced in SQL. |
| `backups.go:90-95` **UpdateBackup** | **A rule encoded by absence, and it is live.** The `SET` list is `destination_id, container_mode, cron, timezone, keep_latest, enabled` — `kind` is not in it. But `BackupScheduleService.Reconcile` (backupschedule.go:147-149) assigns `b.Kind = in.Kind`, `normalise` then validates the new kind (dump-needs-a-database, volume-needs-a-volume, backupschedule.go:215-245), and `UpdateBackup` silently discards it. A kind change is accepted, validated and never persisted. This is exactly the failure `sqlite/tiles.go:184-200` documents at length for the old whole-row `UpdateTile` — "compiled, ran, returned nil and persisted nothing — a write that looked like it worked" — reproduced on a table that never got the `TileConfig` treatment. |
| `backups.go:120-123` **ListBackupRuns** | `limit <= 0 → 20`. |
| `users.go:33-44, 46-75` **GetUserByEmail / foldEmail** | Case folding on read (`COLLATE NOCASE`) and on write. `service.NormalizeEmail` (auth.go:34) is the same rule one layer up and its comment calls this "belt and braces" — so it is acknowledged duplication rather than a hidden rule, but it is two enforcement points. |
| `workitems.go:48-58` **SupersedeQueuedWorkItems** | The whole supersession policy: queued-only (a running job must be allowed to finish), older-by-rowid-only (not "everything but me", which deadlocked two simultaneous enqueues), and `dedupeKey == ""` means never dedupe. Three rules, no service equivalent. |
| `workitems.go:67-76` **ClaimWorkItem** | The optimistic lock (`AND status = 'queued'` + rows-affected) is the queue's entire concurrency contract. `DeployService`'s type comment (deploy.go:26-30) cites this file as the reason a deployment needs no claim of its own — a service reasoning about correctness from a SQL file. |
| `secretlinks.go:44-52, 57-83` **ClaimSecretLink / BurnDropLink** | The one-shot burn (`AND state = ?` + rows-affected) and the burn-plus-writes transaction. `RevokeService.Claim`/`BurnDrop` forward blind. |
| `servers.go:71-95` **BurnServerJoinKeys / BurnJoinKey** | `used_at IS NULL AND expires_at > ?` is the join-key validity rule; `repo.JoinKey.Spent()` (models.go:337) is a *second* spelling of it on the model. Two definitions of "spent". |
| `tiles.go:263-277` **RecordTileRun** | `tiles.status` is deliberately left untouched, because every card that rolls tiles up takes the worst status in the group (`graph.WorstStatus`), so writing a failed run there reddened the whole environment and stack card for one bad tick. Which column an outcome lands in, and why, decided in the store. |
| `jobs.go:41-50` **OpenCronRun / ListOpenCronRuns** | `finished_at IS NULL` defines "in flight"; the comment notes `tiles.status` never says running for a cron, i.e. the query is the authority. |
| `intended.go:28-32` **ClearDeclaredIntended** | `AND value = ''` encodes "empty value means the config file wrote this row" (models.go:180-186). A sentinel-value convention enforced in a `DELETE`. |
| `members.go:54-57` **ListInvitesByOrg** | `AND used_at IS NULL` — "outstanding" defined in SQL, where `MemberService.ListInvites` (member.go:194) just forwards. |
| `notifications.go:33-36` **MarkAllNotificationsRead** | `AND read = 0` (harmless optimisation, listed for completeness). |
| `store.go:40-85` | `hamr auth.SessionStore` implemented directly on the sqlite store, with no service above it. |
| `variables.go:86-102` **DeleteResource** | Hand-cascades `resource_outputs` and `resource_bindings` ("SQLite runs without foreign keys here"). |

One shape recurs across `deployments.go:21-29`, `stacks.go:202-213`,
`jobs.go:26-28`, `backups.go:116`, `notifications.go:20`, `members.go:53`,
`staged.go:16-20`: **ordering by `rowid` instead of `created_at`**, because
`created_at` is TEXT and two timestamp formats coexist. Not business logic
(it is a storage fact), but `staged.go:16-20` flags its own case as
load-bearing — staging replays in order and later entries win — so the
*correctness* of `TileService.Staged` (tile.go:489) rests on a comment in the
persistence layer.

## audit.Record call sites outside service/

The prior said 14. The actual count is **13**. Inclusion rules, stated because
a count that silently differs on them is worse than a different count: no
`*_test.go` file calls it (none exist); `store/audit/audit.go:24`, the
definition, is not counted; the two calls *inside* `service/` (variable.go:200,
:210) are not counted.

| # | Call site | Actor string | Action |
|---|---|---|---|
| 1 | `handlers/middleware/audit.go:38` | `AuditActor(c)` | `Reveal` |
| 2 | `handlers/middleware/audit.go:41` | `AuditActor(c)` | `Edit` |
| 3 | `handlers/middleware/audit.go:53` | `AuditActor(c)` | caller's (`AuditServeValue`) |
| 4 | `handlers/api/v1/variables.go:65` | `auditActor(c)` | `Read` |
| 5 | `handlers/api/v1/variables.go:163` | `auditActor(c)` | `Read`, name `"*"` |
| 6 | `infra/managedtiles/managedtiles.go:442` | `"system:managed-tile"` | `Set` |
| 7 | `config/orgconf/orgconf.go:735` | `"config:"+org.Slug` | `Set` |
| 8 | `config/orgconf/orgconf.go:755` | `"config:"+org.Slug` | `Set` |
| 9 | `config/sharelink/sharelink.go:174` | `"drop-link:"+l.ID` | `Set` |
| 10 | `config/sharelink/sharelink.go:214` | `"share-link:"+l.ID` | `Share` |
| 11 | `config/envops/envops.go:264` | `"system:env-clone"` | `Set` |
| 12 | `config/stackconf/apply.go:1841` | `"config:"+stack.Slug` | `Set` |
| 13 | `config/stackconf/apply.go:1882` | `"config:"+stack.Slug` | `Set` |

Three further files import `store/audit` without calling `Record`; they reach
the trail indirectly and are **not** back doors:
`handlers/web/handler/app/handler.go:982`,
`handlers/web/handler/org/settings.go:325` and
`handlers/web/handler/project/handler.go:1751,2000` all call
`stackrmw.AuditServeValue`, which routes to site #3; the same three files read
through `AuditService.For` (`app/handler.go:950`, `org/settings.go:296`,
`project/handler.go:1731,2057`).

Two notes for the agreed `AuditService` write method:

- **The actor vocabulary is already forked five ways** —
  a bare email and `api:<key>` (both produced by `service.Actor.Audit()`,
  tilelifecycle.go:65-79), plus `system:<what>`, `config:<slug>` and
  `<kind>-link:<id>` invented at the call sites above. An `AuditService.Record`
  has to take that vocabulary as a typed parameter, or the five spellings become
  five string literals passed through one door.
- **`AuditService`'s own doc comment argues against this** (service/audit.go:11-14:
  "A service that owned the write would put one hop between an action and its
  record"). The comment needs rewriting when the method lands, or the next sweep
  reads it as a decision that still stands.

## Clean files

No finding. Grouped by why.

**Rules correctly owned, no duplication found** — `service/access.go`,
`gate.go`, `tenancy.go` (one finding, listed above, otherwise clean),
`auth.go`, `apikey.go`, `member.go`, `container.go`, `domain.go`,
`domain_auto.go`, `domainresource.go`, `environment.go`, `stack.go`,
`slice.go`, `tile.go`, `tilevalidate.go`, `tilediff.go`, `tilelifecycle.go`,
`managedinstance.go`, `backupdestination.go`, `backupschedule.go`,
`registry.go`, `release.go`, `plan.go`, `prenv.go`, `node.go`, `connector.go`,
`imagewatch.go`, `variable.go`, `deploy.go`, `settings.go`, `admin.go`,
`storage.go`, `revoke.go`.

**Thin by design, nothing to decide** — `service/audit.go` (read-only),
`notification.go`, `tiletelemetry.go`, `rows.go` (the one deliberate
forwarder, documented as such), `workitem.go` (documented forwarder; only its
missing read half is a finding).

**Subpackages, all clean** — `svcerr/svcerr.go` (error vocabulary, stdlib
only), `scheduler/scheduler.go` (reload policy, one owner, 28 call sites
collapsed), `mail/mail.go`, `notify/notify.go` (owns the notification write
and the per-user preference default — correct home), `proxy/proxy.go` (owns
every Traefik write; 44 sites collapsed to one).

**Store, clean** — `repo/repo.go` (interface only), `repo/user.go`,
`repo/slug.go`, `repo/giturl.go` (pure validators, correctly below the
services that call them), `sqlite/provisions.go`, `sqlite/audit.go`,
`sqlite/store.go` (bar the session note), `store/audit/audit.go`,
`store/db/db.go`, `store/db/dumpschema/main.go`.

**`repo/models.go`** — the row structs plus a set of predicates
(`ConfigManaged`, `UIEdits`, `IsManaged`, `IsVolume`, `HasImageUpdate`,
`Dead`, `Spent`, `Done`, `Global`, `VisibleTo`, `HasScope`) and three
validators (`ValidateNodePositions`, `ValidateAnnotation`,
`ValidateGraphGroup`). These *are* domain rules, and models.go is the right
place for them: they are pure, they sit below every service, and
`VolumesAttachedTo` (:798) documents exactly why — four copies existed in three
packages that cannot import each other. Not flagged. The one to watch is
`JoinKey.Spent()` (:337), which duplicates the `WHERE` clause in
`sqlite/servers.go:74,88`.

**`store/testdb/`** — seven files, all test doubles. Four of them
(`workitems.go:16-20`, `noderows.go:12-16`, `registries.go:12-16`,
`runrows.go:14-18`) carry the same explicit caveat: *"the methods it doubles
carry no rule today, and if one of them grows one these tests should fail
rather than quietly keep passing."* That caveat is the tripwire for every
forwarder in the hollow-services table — it is already written down, and it is
currently true.

**`envcolor/envcolor.go`** — domain rules, correctly placed, so not a finding.
`Map` (:73-118) does branch on domain state (`bySlug` gives production→rose,
staging→amber, dev→violet at :24-28; `e.Type != "ephemeral"` decides whether a
named slug claims its hue at :82; a file/stack/org/default precedence ladder at
:103-113) and `Valid` (:33) validates domain shape — rubric rules 1 and 2. It
is a leaf package below both `EnvironmentService.Update` (environment.go:281,
which uses it to validate) and the view layer (which uses it to resolve), which
is what lets both reach it. Noted so a later sweep does not re-flag it.
