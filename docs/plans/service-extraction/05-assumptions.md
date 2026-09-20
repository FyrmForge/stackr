# Assumptions taken without asking — points 7 to 11

The developer said: churn 7, 8, 9, 10, 11; make the best assumption for every
question rather than stopping; record them here. One entry per call that a
person could reasonably have decided the other way. Each says what was
chosen, and what the other branch would have been, so reversing one is a
read of this file and not a re-derivation.

Format: **[point] question — decision.** Why. *Other branch:* what reversing
it looks like.

---
## Point 7a — the live-state predicate and two access gates

**Where the shared predicate lives — `internal/deploystate`, a new leaf
package outside `internal/stackrd`.** The plan says "one `IsLive`/terminal
predicate, exported" and "the CLI stops hard-coding it". But the CLI
deliberately imports no `stackrd` package (`internal/cli/cmd/plan.go:77`
says so), `infra/deploy` may not import the service layer (D7), and the
predicate is needed in all three. A leaf with no dependencies is the only
place all of them may point at; `internal/netaddr` is the existing
precedent for a shared leaf. *Other branch:* put it on `DeployService` and
leave the CLI hard-coding its own copy, which is the duplication the row
exists to close.

**`IsCancellable` was added beyond the two the plan named.** `engine.go:310`
guards on queued-or-`waiting_ci`, which is neither the live set (it excludes
`running`, cancelled through its context one branch above) nor the terminal
set. Left as a bare string comparison it would be a fifth copy in waiting.
*Other branch:* inline the two constants at that one site.

**A deployment whose tile row is gone is now a 404 on the panel, not a
readable row.** `web/deployment/handler.go:44-52` skipped the membership
check entirely when `GetTile` returned nil, so any logged-in user of any org
could read that deployment and its logs. There is no other row to check
tenancy against, so refusing is the only safe answer. *Other branch:* keep
serving it and accept the cross-tenant read.

**`loadDeployment` in the API grew a `write bool` rather than a second
loader.** It mirrors `requireTile(c, id, write)`, which is the established
shape in that file. Cancel passes true — that is the SEC row — get and logs
pass false.

## Point 7b — StackService

**`Bind` does not run the plan.** The plan's method list has `BindConfig`
doing "connector-in-org, staged-row drop, plan run". The plan run was left at
the caller: the panel needs the plans back in order to flash what they say,
an org config apply plans the stack a moment later on its own ("the stack
planner takes it from here", `orgconf.go`), and folding the run in would have
meant either a three-valued return or a plan failure that 500s a request
whose write already succeeded. *Other branch:* fold it in and have `Bind`
return `([]*repo.ConfigPlan, planErr, err)`.

**This also removed the `ConfigPlanner` interface** that a plan-running
`Bind` would have needed, so `StackService` has one interface less than the
plan implies.

**`moved:` writes the slug only, through a `Reslug` method, rather than going
through the full rename.** The `[B]` row is that the path wrote Name and Slug
both, from the slug. The full rename would also stop and redeploy every tile,
which is wrong here: `moved.go`'s own comment says the stack's own apply runs
immediately afterwards and rebuilds them, and that apply is the only thing
that knows the display name the file wants. *Other branch:* call the full
rename and let the tiles roll twice.

**`Update` is one method for the name and the description, not a `Rename`.**
The only caller is the API's `PATCH /stacks/{id}`, which takes both in one
body, and a description edit must not be gated as structural while a rename
is. The gate is asked only when `Name` is set.

**`Create` takes `orgID` as an argument rather than on `CreateStack`.** Who
may create in which org is the caller's question — the panel resolves it from
the active-org cookie, the API from the body or the key's org, orgconf from
the file's org — and none of that is the service's business.

**`OrgDeclared` is a field on `CreateStack` and on `BindConfig`.** Only
orgconf sets it. It is not derivable inside the service: the same create call
is a person's and a file's, and the flag is what tells the two apart later.

**No audit rows for stack writes.** `store/audit` is a secret-value trail
(its package comment says so, its vocabulary is reveal/copy/read/set/delete).
Stacks have no audit surface to add to, and inventing one is a feature, not
this refactor. *Other branch:* widen the audit vocabulary, which is point 15
or 16 territory.

**`orgconf.Runner.StackSvc` is required, not nil-checked.** Every other
optional field on that struct is nil-checked for tests that only diff, but a
nil here would be a silent skip of the rules the point exists to enforce.
Tests that only diff never reach `applyStack`.

## Point 7c — DeployService + ReleaseService

**`ClearWaiting` was not extracted; it did not need to be.** The plan lists
`ClearWaiting(ctx, ownerKind, ownerID, name)` as a new DeployService method.
`infra/deploy/waiting.go` already has it, in three scopes (tile-set, env, org),
already the only implementation, already called from every writer that owes it
after point 6. Moving it would have been motion. *Other branch:* wrap it for
the sake of the method list.

**`RedeployIfRunning` moved out of VariableService rather than being written
fresh.** Point 6 had put a private copy there; the panel had three more
(variables, volume attach, storage attach/detach) and the API two. All six go
through one method now.

**Two of those six used to deploy unconditionally** — the API's volume create
and delete, and the panel's volume attach — so attaching a volume to a stopped
service started the service. They are now redeploy-*if-running* like the other
four. A stopped service picks the mount up on its next deploy either way.
*Other branch:* keep two rules, one for volumes and one for everything else.

**A plain promote runs on the work queue, under a new `config.promote` kind.**
The plan decided "promote never runs inline on a request context" and this
follows it — but honestly: `Applier.Promote` is a walk of the upper rungs'
tiles and a queue push per tile, all database work. It was not demonstrated to
blow the 30-second budget, and the failure `job.go:52-57` documents is the
*apply*, which clones repos and builds images. What the queue does buy is a
row to record the failures against, which inline had nowhere to put.
*Other branch:* leave it inline and skip the new kind.

**That promote job fails on restart rather than requeueing.** An apply is
convergent — re-run it and it finishes the job. A promote is a decision about
which commit an environment runs, and re-taking it after a restart could move
an environment backwards past whatever was promoted in between.

**Its dedupe key is `stackID:envSlug`.** Two people promoting to staging at
once is one promote. *Other branch:* key on the commit too, so promoting two
different commits to one env races rather than collapsing.

**A promote now refuses a commit nothing has built.** `02` flags that the API
and `Applier.Promote` accept any string and never check. The check is "some
tile in this stack has a finished deployment of this sha". It is a scan of the
stack's deployments, which is a handful of indexed reads.
*Other branch:* let the tag 404 at the registry, per tile, after reporting
success.

**Promote force is the panel's checkbox, off by default.** The plan decided
this; the checkbox only appears on the apply-and-promote button, since force
means nothing without a plan.

**`ReleaseService.List` was not built.** The plan lists `List`. The API's
release listing and the panel's commit log are two different views — the panel
joins GitHub commit data and renders a log, the API returns per-commit
built/building/running-on — not two implementations of one rule. The one thing
they genuinely shared was the live-status set, and that is closed in 7a.
*Other branch:* build a common release projection and have both render it,
which is a display refactor rather than a rules one.

**The upper-env gate for image auto-update is `envnet.UpperEnv`, called from
`infra/imagewatch` directly.** The plan decided the gate applies, and the
handoff notes say `infra/` may not call the service layer — the advisor
flagged these as a conflict. There is none: `envnet` is itself under `infra/`,
so the watcher honours the gate without pointing up. The skipped check records
an `ok` cron run saying so, rather than going silent. *Other branch:* defer it
to point 15.

**Stack export was deduplicated into `stackconf.ExportStack`.** Listed in `2.4`
as a copy-paste; twenty-five identical lines in the panel's download and the
API's GET, the ladder-order fixup included.

## Point 7d — PlanService, and the StagingService that was not built

**`StagingService` was not built.** The plan lists it with seven methods and
justifies it as the owner of "the editGate decision". Staging has exactly one
surface — the panel; there is no API for it — so there is no second
implementation to converge, and the edit gate already has an owner:
`GateService`, from point 3a. What did get done is the thing that was actually
duplicated: `web/app/handler.go`'s private `editGate` was a second copy of
`GateService.Gate(GateFieldEdit, SurfaceCanvas)`, written before the gate
existed, and it now forwards to it. *Other branch:* wrap the four staging
handlers in a service with no second caller.

**Staging discard did not gain a gate.** `02` flags "staging discard has no
gate" beside staging apply, which does have one. Apply needs the gate because
it performs structural writes; discard deletes rows that the gate has already
made unapplicable, and `StackService.Bind` deletes the same rows wholesale on
purpose. Gating discard would leave a config-managed stack's pending set
permanently stuck on screen with no way to clear it. *Other branch:* gate it
and add a separate escape hatch.

**`replanAsync`'s dropped errors are already closed.** `02` names
`web/project/handler.go:2525-2538` as dropping errors on
`context.Background()`; point 6 replaced it with `Planner.Replan`, which
detaches its own context and logs. Nothing to do. Same for the duplicated
pending-moves filter — one implementation, in `apply.go`.

**Plan preview keeps its org-write gate; the route's scope changed to
`config:apply`.** `02` flags the mismatch (registered `config:read`, gated
write) without saying which wins. There is a deliberate prior decision here
with a test on it — `TestPreviewRequiresOrgWrite`, "it used to take the read
path and so ran for any org viewer holding config:read" — so the gate is the
intent and the registration was the mistake. `config:apply` is the scope
`POST /config/plan` already uses and its description is "re-plan, approve and
reject". *Other branch:* add a fourth `config:write` scope between read and
apply.

**The pending check answers 409 everywhere.** The panel answered 400. Two
people pressing Apply is a race, not a malformed request.

**`GET /org-config/plans/:id` is new, and `stackr org config approve` now
reads before it confirms.** The stack side has had read-one since it existed;
the org side had only the list, and the CLI's own help text admitted it was
approving something nobody could read. It now confirms only for a destructive
plan, which is what `stackr plan approve` does. *Other branch:* leave the
route out and keep the unconditional confirm.

**`PlanService.Get`/`List`/`Export`/`SetInput` were not built.** They are
store reads with no rules attached, and `Export` is now one function in
`stackconf`. A service method that forwards a store call adds a hop and hides
nothing.

## Point 7e — the ApplyEngine, and how little of it was left

**Most of 7e had already landed in points 3 to 6.** The plan's list —
"each createTile/updateTile/deleteTile/syncDomains/applyDomainRes/applyVars/
ensureSecrets/applyBackups/env write/RenameStack/applyMoves/applyMiddlewares/
createSlice becomes a call into the owning concept service" — was written
before those points ran. By the time 7e came round: `deleteTile` already goes
through `TileService.TearDown`, `updateTile` asks `service.Changed` which side
effects it earns and asks `ManagedInstanceService.AfterWrite` for a database,
`createSlice`/`updateSlice` go through `SliceService`, `applyVars` and
`ensureSecrets` write the audit row and release the parked deploy that point 6
gave every other writer, the env-settings block writes only what the file
declares, and `RenameStack` is `StackService`'s renamer. `applyBackups` is
point 8's.

**What was left, and was done: one validator.** `createTile` checked the cron
expression and nothing else, and `updateTile` checked nothing. Both now run
`TileService.Validate`, the same one the panel and the API run. A config file
could declare a resource limit, a port list, a storage attachment or a
placement that both other surfaces refuse outright, and the refusal arrived at
deploy time as a failed container with the plan already marked "applied".
*This can break an apply that used to pass* — deliberately, and pre-release,
where a file that was always invalid now says so at plan-apply time instead of
in a container log.

**`createTile` still builds the row itself rather than calling
`TileService.Create`.** `Create` applies the panel's defaults, its naming
rules and the structural gate. The gate would refuse — the stack is
config-managed, which is the whole reason this code is running — and the
defaults are exactly what the file is there to state explicitly. Routing
through it would mean a `Create` with every part switched off by a flag, which
is not one implementation, it is two behind one name. The rules they genuinely
share are the validator, and that is now shared. *Other branch:* add
`TileService.Adopt`, the way `EnvironmentService.Adopt` exists.

**`applyVars` still writes its own rows rather than calling
`VariableService`.** Every service write ends in `after()`: a replan and a
redeploy. Calling it from inside an apply would replan the stack being applied,
from inside the apply, recursively. The side-effect half is already shared —
`Applied`, from point 6 — and the write half is six lines. *Other branch:* a
`SetFromConfig` on VariableService that skips `after()`, which collapses the
six duplicated lines at the price of a second write path with a flag.

## Point 8 — BackupScheduleService + BackupDestinationService

**A dump of something that is not a database is now refused.** The plan says
"three kind derivations"; it does not say what the third should answer. The
API accepted `kind: dump` for a plain service, which produced a schedule that
failed on every run for ever. A volume tar of a database stays allowed — that
is a real choice. *Other branch:* accept it and let each run fail.

**The gate is `GateFieldEdit`, not `GateStructural`.** The plan says "the
`backup:` key is file-owned, so `editGate` applies on both surfaces". A
schedule is a field of a tile the file declares, not a piece of the stack's
shape, so a stack that says `ui_edits: stage` stages a backup edit like any
other owned field rather than refusing it. *Other branch:* structural, which
refuses every panel backup edit on a config-managed stack outright.

**A destination that does not resolve answers not-found, not invalid.** The
API already did this (there is a test) and the panel flashed the raw error.
The destination is the tenant boundary on this path — the tile is already
known to be the caller's, the bucket credentials are what could not be — so
"exists but is not yours" and "does not exist" have to be one answer.

**`Users` reports `stack-slug/tile-slug`.** The two copies were otherwise
identical. Both spelled it this way; the summary in `02` says "names the
stacks", which would have been less useful.

**Cron and timezone became pointers on the API patch body.** That is what
makes a timezone clearable, which the plan asks for. It means `"cron": ""`
now clears the cron rather than being ignored — deliberate, and the panel has
always behaved that way because a form posts every field.

**`BackupRunService` was not created.** `2.10` says it "= infra/backup.Service"
— it already exists, under that name, below the line. The round-2 note that
`quiesce` and `stopFor` should go through `TileLifecycleService.Stop/Start`
is tagged `g-boot-workers` and is a different concern from schedule rules;
not done here. *Other branch:* fold it in and have a backup run flip
`tiles.status` through the lifecycle service.

**`DropForTile` was not built.** The plan lists it as "what every tile-delete
path calls". The rows go by cascade and the reload is `SchedulerService`'s,
which point 1 already put on every delete path. A method that deletes nothing
and reloads what something else already reloads is a hop.

**`ListDestinations` in the settings package stayed as a function.** Other
packages call it by name; it is now three lines forwarding to
`BackupDestinationService.AtScope`, so there is still one rule.

**A config apply now owns the tile's whole schedule list.** The plan decided
this. Planning and applying `cur[0]` only meant a second schedule made in the
panel was invisible to the plan and then deleted anyway when the block went.
The plan row now says how many extra ones the apply will remove.

## Point 9 — VolumeService that turned out to be TileService, plus StorageService

**No `VolumeService` was created.** A volume is a tile, `TileService` owns
tiles, and after point 3 it already held every rule the plan lists for
`VolumeService.Create`: the name grammar, the reserved-slug check, the
duplicate check, the target-kind rule (`checkAttach`: a service, unmanaged,
same environment), the absolute-mount-path rule, the stage-versus-direct
decision and the target's redeploy. What was missing was a second creator
going through it. The API's `createVolume` was that second creator, with its
own version of five of those rules and a target rule that accepted a cron;
it is now four lines building the row and one call. *Other branch:* a
`VolumeService` wrapping `TileService` for the volume kind, which is a second
name for one thing.

**`volume_name` and `max_size_mb` moved into `TileService.Validate`.** Only
the API accepted them and only the API checked them. The name lands in a
docker bind string verbatim, so an unchecked one can name the node's root
filesystem — that check now runs on every path, the config file included.

**Deleting a service now orphans its volumes instead of leaving a dangling
pointer.** `TileService.Delete`'s own comment names this ("the tile row does
not cascade to its volumes, which is point 9's mess"): the volume kept an
`attached_tile_id` pointing at a row that no longer existed, which made the
attached-guard unable to tell a live owner from a dead one. `TearDown` now
clears the pointer and the mount path on the way out. Orphaned, not deleted —
the docker volume holds data, and deleting the row is how you lose track of
it. *Other branch:* cascade in SQL, which the frozen `001_initial` forbids.

**Deleting a volume now redeploys its former target from every path.** Only
`DELETE /volumes/:id` did. `DELETE /apps/:id` with a volume id and the panel's
delete did not, so the service kept mounting a volume that was gone.

**`repo.VolumesAttachedTo` is a pure filter on the `Tile` type, not a
`VolumeOwnership` package.** The plan calls for `service/volumeown/` as a
leaf (D1). Three of the four copies are under `infra/` — the placement
resolver, the deploy engine's bind builder, the volume mover — which may not
import the service layer, and the fourth is the API. A two-line predicate over
a slice needs no store and no package; it goes beside the type every one of
them already imports. `OwnerOf` and `NodeOf` are the class-C raw-volume rows,
which `03` puts out of scope. *Other branch:* the leaf package, with three
`infra` imports pointing at it.

**`ConvertLegacyMounts` was deleted, not converted into a caller.** The plan
says to check `volumes_migrated` first and that deleting it outright is fine
if the flag is set everywhere. It is a one-shot upgrade for installs that
predate volume tiles; AGENTS.md says the project is pre-release with no
production installs, no data migrations and no upgrade paths, and
`001_initial` was frozen after volume tiles existed. A fifth volume-tile
writer with its own slug rule, running on every boot, for nobody.
*Other branch:* keep it and route it through `TileService.Create`.

**The storage attach kind rule is "not a volume tile", not "service only".**
The config path allowed a service and nothing else, the API allowed any kind
and the panel checked nothing. Service-only is too narrow in both directions:
a cron mounts a share to write into, and `storagetiles.ValidateAttach`'s
local-backed rule exists precisely so a managed instance can attach one. What
genuinely cannot is a volume tile, which has no container of its own. The rule
lives in `TileService.Validate`, so every write path gets it — the panel's
attach and detach now go through `TileService.Update` rather than writing the
row themselves.

**`ServerID` defaults to `"local"` in the API handler, not in the service.**
The hard-coding was the bug, but the API has no node in its path and a pool
has to live somewhere; the handler now takes `server_id` from the body and
falls back to the manager, and the panel passes the node whose page it is.

**Sub-path names are slugified and then checked.** The API's ordering, as the
plan decided.

**`StorageService.Attach`/`Detach`/`RemovePath` were not built.** Attach and
detach are `TileService.Update` — the attachment is a tile column. Removing a
sub-path is a consumer scan (now `Consumers`) plus a row delete; the scan is
shared, the delete is two lines in each handler.

## Point 10 — RegistryService, PREnvService, and the ConnectorService that was one line

**`RegistryAdminService` and `OrgRegistryService` are one `RegistryService`.**
They share the managed-registry row — the admin half decides whether it may
be deleted, the org half cannot mint a credential without it — and neither is
more than a handful of methods. Two types would have meant one holding a
reference to the other for that single question.

**`ConnectorService` was not built.** The plan lists `Connected`, `ForTile`
and `CheckOwned` as replacing "four inline filters". The filter is
`cn.OrgID != <something>.OrgID`: seven one-line comparisons across seven
packages, answering four different questions (is this the tile's org, the
stack's, the org config's, the hook's). Wrapping a field comparison in a
service call adds a hop and hides nothing. What was actually broken is the
two that failed *open*, and both are fixed where they live:

- The planner skipped the check whenever `OrgConnectors` was empty, which
  conflated "the lookup failed" with "the org owns none" — so a file naming
  another org's connector planned clean on any org with no connectors of its
  own. `State.ConnectorsKnown` separates them; the blip still skips, the
  empty org now refuses.
- `githubapp.connectorForTile` returned nil on a refusal exactly as it does
  when no connector is configured, so the clone ran unauthenticated and the
  only trace was a git error. It logs the refusal now. It still returns nil —
  `CloneAuth` has no error channel, and an unauthenticated clone of a private
  repository fails on its own.

*Other branch:* the service, with seven call sites rewritten to ask it.

**Deleting a connector is refused while anything still names it.** The plan
asks for the reference check; it covers the org's config binding, each
stack's, and every tile's, and names them. Refused rather than cascaded: the
bindings are a person's configuration, and silently clearing them is worse
than a message.

**Org registry writes are owner-level on the API too.** The plan decided this
and it is the [SEC] row: credential create, credential delete and tag delete
took any member holding a `stacks:write` key, where the panel has always
required an owner.

**`PRConfig` moved from `config/envops` to `store/repo`.** It had to: the
service layer needs it, `envops` imports the service layer, and
`infra/githubapp` reads it too when it decides whether to post deploy
feedback. `repo` is the one package all three already import. *Other branch:*
a fourth leaf package for a four-field settings blob.

**`PREnvService.Update` gates only `comment` and `status`.** Those are the
keys the file writes. `enabled` and the webhook secret are the panel's on any
stack: the file can decline to build a preview, but the webhook credential is
not its business, and gating `enabled` would leave a config-managed stack
unable to turn previews off from the panel at all. *Other branch:* gate the
whole blob, per "the `backup:` key is file-owned" reasoning.

**The file's `pr_envs.enabled` is now persisted.** It was read to decide
whether to build and then thrown away, so the panel toggle and the file
disagreed with no way to see which was in force.

**The stack-scoped hook gained both halves of the connector route, not one.**
The plan names `stackTracksRepo`; `02` names `updatePlanComment` alongside
it. A webhook pointed at a stack by hand now ignores a repository none of its
tiles track, and posts the plan preview when the stack's config repo is the
one that moved.

**`PatchCredentials` and `SetManagedDomain` were not moved.** The domain path
already goes through `ProxyService.SetRegistryDomain` on both surfaces (point
2 closed that row). The credential patch is two assignments on a row the API
alone exposes.

## Point 11 — Org, Member, Auth, APIKey

**`AccessService` was not built, and that is point 15.** `2.5` carries a
long brief for it — one principal type, resource-rooted checks, a
per-operation table, revocation. It is its own point in `03` (15) and the
brief says so. What point 11 did is the individual authorisation rows it
names, each where it lives.

**`Active` is checked at principal resolution on both surfaces.** The [SEC]
row: deactivating a user closed nothing, because `Active` was read at login
and nowhere else. A session's subject loader now returns nil for an inactive
user, and `KeyAuth` answers 401. *This logs out anyone deactivated while
signed in*, which is the point.

**API keys still have no org column and scopes are still checked at mint
time, not use time.** The plan's pick says use-time validation against
current roles in the target org. That is the AccessService design — it needs
the principal type and the per-operation table to have anywhere to live — so
it belongs to point 15 with the rest of the brief. What holds today is the
per-request `requireOrgWrite`/`requireOrgOwner`, which point 11 tightened on
the rows that were wrong (org plans, org defaults, registry credentials).
*This is the largest thing in point 11 left undone.*

**Org plans are owner-only over the API, including plan and preview.** The
plan decided approve; plan and preview go with it because a plan is what an
approval acts on and previewing one reveals the whole org's desired state.
Listing stays at member level — it shows summaries, and the panel lists for
members too.

**An org plan still applies on the request context.** `02` flags this as a
[B]. It was queued and then reverted: both surfaces redirect to the org's
*post-apply* slug, because an org plan can rename the org and the setup
wizard's next step lives under the new one. Queuing it needs a progress
screen for org plans — the equivalent of the stack plan page — and that is a
feature, not a refactor. The failing tests were the wizard's own journey,
which is exactly the coupling.

**This is now point 17 in `03-refactor.md`**, specced rather than left as a
note: the redirect moves to the org *id* (which `loadOrg` already accepts,
so it survives the rename) and the progress banner is the stack plan page's
lifted across. That turns out to be small — the thing that read as a feature
was the screen, and the screen already exists one directory over. Still the
one named row in 7–11 not closed.

**Email folds in the store, and the fold is a `COLLATE NOCASE` lookup plus a
lower-cased write.** The plan asks for the fold and a one-time migration to
collapse existing case-variant rows. No migration was written: `001_initial`
is frozen (AGENTS.md, 2026-09-18), the project has no production installs,
and the NOCASE lookup already finds a row written before the fold. A
duplicate-by-case pair would now be ambiguous on lookup rather than newly
broken — it was already two accounts neither of which could reliably log in.
*Other branch:* a migration that merges them, which needs a rule for whose
password wins.

**One password rule, hamr's `PasswordStrength`, enforced in `AuthService`
rather than in the handlers.** The invite-accept and change-password forms
asked for eight characters and the register form ran the full check, so the
weakest password the product accepted depended on which form set it. Putting
it in `Register` and `ChangePassword` means the handlers cannot skip it.
*This makes some existing passwords unchangeable to their current value* —
intended.

**`MemberService.Accept`, `Resend`, `Reinvite` and `Revoke` were not built.**
They are single-surface (the panel) with no second implementation to
converge. `Invite`, `SetRole` and `Remove` are the three that existed twice
and disagreed.

**`OrgService` was not built.** Its methods in `2.5` — `CreateDraft`,
`Rename`, `Delete`, `FinishSetup`, `SetDefaults`, `Bind` — are the setup
wizard, which is one surface. The two rows that were real are the org
defaults gate (now owner + managed-refused on the API, matching the panel)
and the rename anti-squat rule, which `orgconf` skips. The anti-squat rule
is *not* fixed: it is a panel-only check on a path `orgconf` reaches by a
different route, and moving it means deciding whether a config file may
claim a name a person could not. *Other branch, flagged:* that decision.

**`APIKeyService` owns the token format and the row; the scope filter stayed
with the scope catalog** as `v1.GrantableScopes`. The catalog is in the API
package and the service cannot import it; the filter is about the catalog,
the row is about keys. Three copies of each became one of each.

**`HashKey` moved into the service and `v1.HashKey` forwards.** The
authenticator has to hash a header with exactly the function that wrote the
row, and that function now lives with the minting.


## Point 17 — the org apply on the work queue

**The stack plan page's apply banner had never rendered.** Point 7 moved the
apply's dedupe key from the stack id to the plan id, and
`handler/project/handler.go` kept asking `LatestWorkItem` for the stack's id,
so the lookup never matched a row and `workLine(nil)` rendered nothing. Fixed
while lifting the banner, because the lift depends on it: the wizard's
self-advance *is* that poll. Outside point 17's stated file list; agreed
before it was touched.

**The banner went into `components/plan.templ`, not into `org/`.** All three
plan screens — the stack's, the org settings one, and the wizard's step 3 —
already render `components.PlanBody` with a `PlanViewCfg`, so the banner is
two fields on that struct (`Work`, `PollURL`) rather than a block copied into
two more templates. `workRunning` and `workLine` moved with it.

**The wizard's self-advance is in its handler, not in the component.** The
spec's phrasing suggested a `DoneURL` on the cfg, but nothing in the template
would have used it: `SetupConfigPlan` sees the finished job and answers
`respond.Redirect`, which hamr already turns into `HX-Redirect` for an htmx
request. One branch in the handler, no new field.

**The wizard's plan screen is addressed by plan id now.** It asked
`pendingOrgPlans` for "the newest plan still waiting", and the plan stops
waiting the instant the apply lands — so the poll after the successful one
would have found nothing pending and bounced back to the binding form. The
poll URL carries `?plan=<id>`; arriving from step 3 without it still falls
back to `pendingOrgPlans`.

**The job records the plan error itself, and `Runner.Apply` is untouched.**
Apply writes the row on two of its exits (the file load, and the collected
teardown failures) and returns bare on the rest — the diff, a plan with
errors, `applyMoves`, the rename refused over registry images, `applyDomains`,
`applyShared`. Inline those at least became a flash; queued there is no flash,
and the row is the only record. `RunApplyJob` writes it on any error, which
double-writes the same message on the two paths that already do. Cheaper than
six edits inside a function the spec says not to change.

**`waiting()` still counts an applying plan as waiting.** The org canvas
banner says a plan needs deciding for the seconds between the approve and the
job finishing. Left alone: the alternative is a fourth status on the plan row
and this is a few seconds of a banner that links to the page now showing the
progress. *Flagged, not fixed.*

**Both plan screens bounce their own poll once the job is done.** Only the
banner swaps on a poll, so the rest of the page stays at the approve-time
render: an applied plan still reading "pending" with a live Approve button
that the next click would 409 on. When the work is done the poll is answered
with a redirect instead — the wizard's to its next step, the settings page's
to itself — and htmx turns that into a navigation that re-renders everything.
Guarded on `HX-Request`, or the navigation would arrive back here and ask for
another.

**The plan's buttons go away for the length of the apply.** The row stays
`pending` while the job runs, so Approve and Reject would both stay live. A
second Approve is harmless (`RunApplyJob` refuses a decided plan), but a
Reject landing mid-run is written and then overwritten by `Runner.Apply`'s
closing `SetOrgConfigPlanStatus(..., "applied")`, and the org gets built by
something the operator just refused. `withoutButtonsWhileApplying` clears all
three URLs while the work is live.

*The stack plan page has the same stale-render problem and does not do this.*
It has had it since point 7; nobody saw it because its banner never rendered.
Left alone rather than widening this diff — it is the stack surface's row.

**The org web handler was never given the work queue.** `h.work` and
`WithWork` survived point 11's revert, but `server.go` never called it, so the
first approve after this change was a 503. Wired at the org handler's
construction.

## Points 13 and 16

- **The web forms flash where the API 400s.** `SettingsService` speaks
  `svcerr`, and the API maps it to a status as usual. The panel's four saves
  post over htmx, which renders an error body nowhere, so a bare 409 there is
  a save that appears to do nothing. `middleware.FlashRefusal` turns an
  `Invalid` or a `Conflict` into a flash and a redirect and leaves everything
  else to the error page. The org defaults page used to return a real 409 for
  a config-managed org; it flashes now. Same refusal, visible to the user.
- **`installspec` is `internal/`, not `internal/stackrd/`.** The installer is
  a separate binary that runs before there is a database. Anything the package
  imports, that binary carries, so it imports nothing but the standard
  library.
- **`AdminService`'s swarm import went away by moving the policy down, not
  up.** The spec said the spec shape it needs comes from InstallerSpec. In
  fact all it held was a `swarm.UpdateConfig` — an update order and a rollback
  action, which are docker concepts. They live in `infra/runtime` now, which
  has one caller.
- **The root-domain grammar is now shared, which tightens the panel.** A
  domain resource can no longer be `localhost`, a bare address or a single
  label. Existing rows are not revalidated; only new ones go through it.
- **`EnsureAgent` returns the first attempt's error and retries behind it.**
  The spec asked for one policy. A request goroutine cannot block for ten
  attempts at fifteen seconds, so the first try is synchronous — the Add node
  modal still gets something to say — and the rest run in the background under
  a flag, so two clicks do not start two loops.
- **Remove still allows an exited system container.** SP3 says keep the
  guard, and the guard is kept for the running one. A stopped agent container
  is a previous generation left by an upgrade; refusing it is the bug that had
  every node accumulating rows no operator could clear.
- **The terminal is refused on the page as well as the socket.** The socket
  refusal is the real one; without the page one the browser opens a terminal
  and then fails its handshake, which reads as a bug rather than a rule.
- **`metrics` takes a function, not an interface.** One implementation, and an
  interface for one implementation is the thing D1 exists to avoid. The
  function exists to break a package edge, not to allow a second reader.
- **`PublishConnection` audits only a changed value.** It runs on every deploy
  of a database tile. Recording every run would put a row per credential per
  deploy into the secret-activity page and bury the writes that mean
  something.
- **`envops.repointRefs` is still unaudited.** It rewrites a reference from
  one resource slug to another, which is not a secret being written. Left
  alone deliberately.
- **`AuditService` did not become a package.** `store/audit` already holds
  `Record` and the action vocabulary, and `middleware.AuditServeValue` already
  is the spec's `ServeValue`, shared by four call sites. Point 16's row was
  the unaudited writers, and that is what was fixed; moving the package would
  have touched every importer for no behaviour.

## Point 15

- **The table is the deliverable; the helpers stayed.** The spec's file list
  names every gate helper on both surfaces. Collapsing forty handlers onto one
  `Require(ctx, principal, verb, resource)` call in one pass is the change most
  likely to open a hole while closing one, and the rows `02` actually flagged
  are all "the two surfaces picked different levels for one verb". So the level
  moved into one table and the helpers ask it. `requireOrgWrite`,
  `requireOwnerOf` and the wizard routes go through `AccessService`; the rest
  still carry their own loading and their own wording, which is where a 404
  that must not become a 403 lives.
- **Verbs are org-rooted, not resource-rooted.** The brief asks for methods
  that take the resource and walk to its org. Every caller already has the org
  — that walk is what `loadStack`, `requireTile` and friends do before they
  gate — so `Require` takes the org id. A resource-rooted call would mean
  AccessService loading rows the handler has already loaded.
- **`svcerr.Forbidden` is new.** The package said a 403 must not explain
  itself, and the reason given was that a refusal must not confirm a resource
  exists. That reasoning is about choosing between 404 and 403, not about what
  a 403 may say once chosen. `ErrForbidden` remains the bare one and is what a
  level refusal returns; `Forbidden` carries a sentence for the cases where the
  sentence is the whole point, like a missing API key scope.
- **The wizard raise is a route middleware, not a handler change.** The two
  steps are served by the same handlers that serve org settings, where member
  is the right level. Raising the handler would have raised both.
- **Websocket rooms are authorized on join, not on the upgrade.** `/ws` stays
  outside the site group: moving it inside would put CSRF, flash and secure
  headers on an upgrade request for no gain, since the join is where a room is
  named. An anonymous socket connects and can join nothing.
- **The subject lookup does not use the request context.** It runs after the
  connection has been hijacked for the upgrade; a cancelled context there would
  refuse every join on the box.
- **Scope grants are still minted against the active cookie org.** Point 11's
  deferral is only half closed. The live role check in the target org is what
  makes a misgranted scope useless, and that now runs through one call; the
  grant itself, and narrowing a key when its user is demoted, are still open.
- **Config-as-code never went through `ValidateResourceHost` and still does
  not.** `orgconf.go:829` and `apply.go:439` write domain resources straight to
  the store. So tightening the grammar cannot break an existing apply — which
  is the reassuring half. The other half is that a `domains:` block in a
  config file has never been checked against the rule the forms enforce. Out of
  scope here; recorded so it is not rediscovered as a regression.
- **A managed stack's defaults forms lost their Save.** `SaveStack` and
  `SaveEnv` refuse a config-managed stack now, and the settings screen rendered
  an editable form and a Save either way, so without this every save on a
  managed stack would flash a refusal. The fields still render — reading what
  the file set is the point of the screen — and the explanation replaces the
  button, which is what the same screen already does for environments and
  colours.

## Point 15's revocation leftover

- **Revocation is a teardown of what cannot re-check, not a second gate.**
  Deactivation is the pattern named in the brief, and what deactivation
  actually does is re-read the user at the boundary on every request — the
  session's subject loader (`handlers/web/server.go`) and the API key
  authenticator (`handlers/api/v1/auth.go`). Nothing is eagerly deleted. So
  the question for a demotion is not "what do we delete" but "what is not
  re-checked", and the answer is share links and websocket rooms.
- **Sessions react already and were left alone.** `middleware.OrgContext`
  re-resolves `ListOrgsForUser` and `GetOrgMember` per request, and an org
  cookie naming an org the user is no longer in falls through to one they are.
  Closing their sessions would also sign them out of every other org, which a
  demotion in one org is not.
- **API keys react already and were left alone.** A key carries scopes, never
  a role; `AccessService.Require` reads the role live out of the *target* org.
  A demoted key is refused at the same instant a demoted session is. The two
  things that could be done instead — deleting the key, which punishes the
  user's other orgs, and narrowing its scopes — are both the mint-time
  question point 15 left for the dev, and are untouched.
- **Share links are the gap, and they are a bearer token.** `SecretLink` is
  redeemed with no session, no key and no role check anywhere in the path, so
  a member who minted one and was then demoted below write, or removed, had
  left a working door open. Revoked, not deleted: the row names fields and
  never holds values, so it is the audit line for the exchange.
- **The cut is "dropped below write", not "role changed".**
  `VerbShareLinkMint` sits on `LevelWrite`, so an owner demoted to member
  still holds the right that minted the link and their links stand. Revoking
  there would take away something the new role grants.
- **A stack move revokes every link under the stack, whoever minted it.** The
  links were minted under the old org's rights and now point at resources the
  new org owns, so neither side's membership answers for them. Org-scoped
  links are untouched: the move did not take them anywhere.
- **`ListOpenSecretLinks` is one query, not a subtree walk.** The store has no
  by-creator and no by-org listing, and the alternative was walking org →
  stacks → envs → tiles calling `ListSecretLinks` per owner. Open links are
  few — they expire — so the whole live set is read once and filtered in Go.
- **`RevokeService.ownerOrg` is not point 18's `TenancyOf`.** It answers the
  same shape of question (an opaque id → the org that owns it) for the four
  owner kinds a link can have, and it is unexported so it does not become the
  resolver by accident. The real one has to answer for every route.
- **Websocket rooms are a nudge, not a teardown, and this is a known
  ceiling.** A room is checked when it is joined and never again, and hamr
  v0.35.0's `Hub` exposes no way to disconnect or enumerate a subject's
  clients — only `SendToSubject`. So `notify.AccessChanged` sends the subject
  an `access` event and `live.js` answers by leaving and re-joining every room
  it declares, which runs the join check again. A client that ignores the
  event keeps its rooms until it disconnects. What that leaks is event kinds
  with no payload — that something in an org changed, never what. Closing it
  properly needs `Hub.Disconnect(subjectID)` upstream.
- **Deactivation does not revoke share links, and still does not.** It is the
  same bearer-token hole in the one path the brief called already correct, and
  `ToggleUserActive` lives in the settings handler with no service and no
  notifier to hang it off. Named here rather than fixed: it changes what
  deactivation means, which is the dev's call, and it is one line once the
  user-admin write moves into `AdminService`.

## The route-verb test (point 18's safety net)

- **The declaration lives in Echo's own `Route.Name`.** Point 18's sketch
  passed the verb to a middleware, which says what to *enforce* but leaves
  nothing a route walk can *read*. Echo already carries a name per route,
  defaulted to the handler's function name, so `.Name = string(service.VerbX)`
  needs no registry, no parallel table and nothing that can drift from the
  routes it describes. The middleware reads the same field.
- **A known-failing list, not a `t.Skip`.** The brief allowed either. A skip
  is no net at all, and the point of landing this first is that a new mutating
  route cannot arrive ungated while point 18 is half done. So all 271 live in
  `knownUngated`, and the test fails both ways: a route missing from the list,
  and a route on the list that has since grown a verb.
- **Three buckets, not two.** Besides "owes a verb" there is genuinely
  unauthenticated (14 routes that carry their own credential — a password, an
  invite token, a share token, a webhook signature, a node join key) and
  personal (8 routes that write the signed-in user's own row and have no org
  to be rooted in). Both are code, in `unauthenticated()` and `personal()`, so
  adding to either is an edit somebody makes rather than a silence.
- **Whether the personal routes belong in the table is left open for the
  dev.** A "self" rung is not a comparison against an org, and `Level` exists
  because a check is a comparison. Recorded in 06 as the open question.
- **The test asserts a verb is named, never that it is the right one.** A
  route naming `VerbOrgRead` for a write passes. Checking the choice means a
  second table of what each route ought to be, which is the two-vocabularies
  problem again.
- **Both surfaces register onto one Echo.** They already do in `main`, the
  route trees do not overlap, and one walk means one inventory instead of two
  lists that have to be kept in step.
- **`/api/v1/nodes/samples` is not in the walk.** It registers only when the
  node service is wired, and the walk passes nil for it. It belongs in the
  unauthenticated bucket — the agent holds the runtime key, not an API key.
- **Nothing was deleted.** No gate helper was touched, no call site moved.

## The owed rig pass

- **The second account was created through the invite link, not the DB.** The
  box sends no mail, and `MemberService.Invite` hands the link back to the
  caller either way — the members page renders a Copy invite on the row. So a
  member exists without touching a root-owned sqlite file.
- **Two sessions, two clients.** One browser profile holds one session, and
  two of the checks need an owner and a member at once. The owner's session
  lives in a `curl` cookie jar (CSRF token read off the page it posts from,
  `Origin` set) and the member's in the browser. That is also what makes the
  deactivation check real: the victim's session was live and untouched at the
  moment the owner flipped the toggle.
- **The panel answers 404 where the brief said 403.** Both are right: 06's
  surface error policy has the panel answer 404 for an owner refusal, because
  a 403 confirms the org exists. The 403 is the API's answer, and that is
  where it was driven.
- **The promote force checkbox and the refused config apply are blocked on the
  same thing, and it is not the fix.** `force` overrides the per-environment
  *apply policy*, so the checkbox only renders on the apply-then-promote form,
  which needs a config plan; an apply needs a bound config. No stack on the rig
  is bound. Recorded rather than worked around: binding one means standing up a
  repository the connector can read, which is the human step point 17 waits on.
- **The rig was left dirty on purpose.** The demoted viewer, the revoked link
  and the member's key are the evidence for the checks above; wiping them would
  make the entry unverifiable by anyone reading it afterwards.

## The rig pass, second run (bound config)

- **The blocker was a bound repository, not a connector.** The connector row
  on `test-org` survived the pave; nothing was bound to it. Binding
  `FyrmForge/stackr-test` / `stackr-org.yml` replans clean ("Nothing to
  change"), so the org was already in the shape the file declares and no stack
  was created or deleted by it.
- **The fixtures are branches, not master.** `rig-badtile` and `rig-promote`
  in that repo, each with one new file. A branch keeps the refusal evidence
  reproducible — `badtile`'s plan stays in `config invalid` forever, which is
  the point — without putting a deliberately broken stack on the default
  branch where the next person planning the org would meet it.
- **A cron tile with a `port:` is the refusal.** `service/tilevalidate.go`
  refuses it with "a cron has no endpoint". It was picked over a YAML error
  because a parse failure is a different code path: the file has to *parse*
  and *resolve* and still be refused, or the check does not touch the
  validator at all.
- **The promote force check is half done, deliberately.** The checkbox and its
  unchecked default are observed live; the force-on/force-off behaviour is
  not. The two preconditions — a commit built on the lower rung, and a pending
  plan for the upper one at that same commit — cannot both be reached by hand
  on a rig with no push webhook: approving the plan to get the build consumes
  the plan, and planning without approving leaves the commit unbuilt, which
  `ReleaseService.built` refuses before `force` is read. Recorded rather than
  bodged, because the bodge (writing plan rows straight into the database)
  would be testing the fixture, not the panel.
- **A stack is created in the active cookie org, not the posted `org_id`.**
  `POST /projects` ignored the org id and put the stack in whichever org the
  cookie named. Noticed while setting the rig up; it is a real row for the
  same reason every other one in this plan is, and it is not point 18's.

## Point 18, step 2a — capturing what the gate helpers enforce

**The route-verb test is not enough on its own.** It asserts a route names a
known verb; it cannot tell `VerbStackWrite` from `VerbOrgWrite`. Putting the
member-level verb on an owner-level route passes it, passes `go vet`, and
leaves the endpoint open one rung — the exact silent failure the dev banned
point 18 over. So what each route enforces TODAY is captured as data first, in
`handlers/web/routelevel_test.go`, and every verb assigned later has to agree
with it. The gate helpers are the only record of current behaviour and point
18 deletes them.

**Captured by AST walk, not by hand.** 271 routes read by eye is 271 chances
to misread. `gatescan` resolves, per handler, the transitive set of gate
helpers its body reaches, and that is joined to the registered routes. Three
findings came out of building it, each one a thing a manual pass would have
got wrong:

- **Enforcement lives in three places, not one.** Handler bodies, per-route
  middleware (`adminOnly`, 40 mutating routes), and the site-wide
  `ReadOnlyGuard`. A capture that only read handler bodies would have recorded
  "ungated" for every `/admin` route.
- **One hop is not enough.** `backups.Run` -> `loadBackup` -> `loadTile` ->
  `RequireStackAccess` is three deep. A one-hop walk called 27 routes ungated
  that are gated fine. Full transitive closure within the package.
- **The two surfaces have different vocabularies.** The panel gates on
  `stackrmw.RequireOrgWrite`/`CanWriteOrg`/`IsOwner`; the API has its own
  `requireStackAccess`/`requireOrgWrite`/`requireOwnerVerb` set, several of
  which take a `write bool` whose value at the call site decides the level.
  Those are resolved from the argument, and the `requireVerb(..., VerbX, ...)`
  family is resolved against `verbLevels` in `service/access.go` so the tool
  and the product cannot disagree about what a verb needs.

**`IsAdmin` is a bypass, not a level.** It appears inside `CanWrite`,
`CanWriteOrg` and most of the API helpers as a short-circuit. Counting it as a
level made every route read "admin". It names the level only when it is the
only gate in the body.

**266 of 271 captured; the other five were misfiled, not ungated.** The
org-home graph routes (`POST /graph/annotations` and the four beside it) reach
no gate because they write `repo.GraphOwner(repo.ScopeUser, u.ID)` — the
caller's own saved canvas layout, not org content. They are personal routes
that read as org ones, and they moved to `personal()` in the route-verb test.
Nothing else in the tree was left unexplained.

**Distribution of what is enforced today:** write 162, admin 54, owner 49,
read 1. The single read is `POST /api/v1/resolve`, which is a lookup that
answers over POST because its input is a path — mutating by method, a read by
behaviour. It needs a read-level verb, not a gate.

**API scopes are orthogonal and were not touched.** `op()` in `api/v1/v1.go`
registers a route's scope beside the route and feeds the OpenAPI spec from the
same call, so deleting role gate helpers cannot silently drop scope
enforcement. `AccessService.RequireScope` still has no callers.

**The capture records the org, not just the level.** A level alone is not the
check. `CanWrite` and the site-wide `ReadOnlyGuard` read the ACTIVE cookie
org; `CanWriteOrg`, `RequireOrgWrite` and `RequireStackAccess` read the org
that owns the addressed resource. Both are "write" on the ladder, and moving a
route between them is a tenancy change that a level-only table would wave
through in both directions — someone who owns org A and only views org B
carries role owner while acting on B's tiles.

So `capturedLevels` has a third column. The answer today is that all 266
mutating routes check the RESOURCE's org (the 54 admin ones are not
org-addressed at all). The 13 handlers that do read the active org are GETs or
personal routes and owe no verb. That clean baseline is the invariant point 18
must not break, which is why it is recorded rather than assumed.

## Point 18, step 2b — TenancyOf

**`Kind` is declared per route, not sniffed from the path.** A bare `:id` does
not say whether it is a tile or a stack, and a prefix table (`/apps/` -> tile)
would be a second place routes are described, drifting from the first. The
route says what it addresses, beside the verb it names.

**Slug-path resolution moved out of the API.** `slugpath.go` resolved
`acme:shop:prod:api` inside the API's four gates. The check now runs in
middleware on both surfaces, so it moved to `AccessService` — two resolvers is
exactly how the two surfaces came to disagree about everything else. The rule
it carried is kept: an id is tried first, and a bare slug below org level is
refused, because slugs repeat across orgs and guessing the tenant is how one
org's script quietly edits another's stack.

**Not found is `("", nil)`, not an error.** The surfaces answer a missing row
differently on purpose — 404 in the panel so an id is not confirmed to someone
who should not know it, 403 in the API — and that mapping stays at the
surface, per "Surface error policy — DO NOT FLATTEN". A store failure is still
an error: swallowing it would read as "no org", which is the same answer as
"not org content", which hands the route to the admin check.

**"No org" is a real answer for three things**, not a resolution failure: a
`KindNone` route (node, container, proxy, server defaults), a backup
destination with a NULL `org_id`, and an instance-level domain resource. All
three carry admin-level verbs, and `Require` does not look at the org for one.

**`KindDomainResource` is listed and scanned** rather than fetched: the store
has no `GetDomainResource`, and the table holds one row per distinct domain
the server answers on. Marked `ponytail:` with the upgrade path.

## Point 18, step 2c — verbs on the routes

**Verb and Kind are two axes, and conflating them was a bug.** The first
assignment pass matched both off one path pattern, which pinned
`KindTile` to `/api/v1/backups/:id` (the id is a backup), `KindOrg` to
`/api/v1/org-config/plans/:id` (the id is a plan) and `KindEnv` to
`/api/v1/stacks/:id/envs/:slug/promote` (the id is a stack). Each would have
resolved a tenancy off the wrong id and 404'd a working route. Split: the verb
comes from what the route DOES, the kind from the noun that owns the
parameter. The generator now asserts the parameter a kind reads literally
appears in the route path, which is what caught all three.

**Two verbs were missing from the catalogue and were added**, rather than
rounding a route up to an owner-level verb it did not have before:
`VerbOrgGraphWrite` (the org's shared canvas — annotations, groups, node
positions) and `VerbConnectorWrite` (connecting a git provider). Both are
write, which is what the panel has always enforced. Point 18 records levels;
it does not change them.

**`VerbOrgDefaults` is reused at stack and environment scope.** Settings are
one operation — set inherited defaults — and `resolveSettingsTarget` already
required an owner at all four scopes. The Kind carries the scope, so a second
verb would have been a second name for one thing.

**`KindDeferred` is the honest escape hatch, and it is five routes.** Creating
a stack (both surfaces), creating a domain resource, creating a backup
destination, and `POST /api/v1/resolve`. In each the org arrives in the body
or is what the request asks to resolve, so there is no tenancy to check before
the handler runs. The middleware still authenticates and still carries the
verb; the org check stays in the handler, where `orgForCreate` already does it
properly. A sixth route here is a decision, not a default.

**ASYMMETRY KEPT, and it is the dev's call:** saving an org domain
(`POST /orgs/:slug/settings/domains`) is write, deleting one
(`.../domains/delete`) is owner. That is what the code does today. Point 18
records levels rather than changing them, so the two carry different verbs
(`VerbDomainWrite` and `VerbOrgWrite`). If they should agree, that is a
one-line change to the assignment — but it is a change in what members may
do, which is not a refactor's call to make.

**Registration makes the two halves inseparable.** The panel's `mutate(...)`
and the API's `opw(...)` mount the check AND record the verb on the route from
one call. A route that carried the check but declared nothing would be
invisible to the route walk; one that declared a verb without mounting the
check would read as gated and be open. Splitting them is the failure point 18
exists to remove, so the registration does not offer the option.

**API scopes are untouched and still run.** `opw` wraps the gate inside
`requireScope`, so a key still needs its scope AND its user still needs the
level. The two are orthogonal: a scope narrows a key below its user's rights,
the verb is the user's own standing.

**One thing the verb table cannot hold, kept beside it:** an org whose wizard
is unfinished takes no writes. `orgReady` is not a level, so the API's gate
calls it directly, as `requireStackAccess` did. The panel's equivalent lives
in `RequireOrgAccess` and is still on the routes; step 3 must not drop it when
the helpers go.

## Point 18, step 3 — deleting the gate helpers, and what stopped

**The helpers do two jobs, and only one of them goes.** `requireTile(c, id,
true)` checks a role AND returns the tile; `ownedOrg(c)` checks ownership AND
returns the org. Handlers use the return value, so the calls could not simply
be deleted — each needed a load-only twin. Those are `a.tile/a.stack/a.env/
a.org` on the API, over `AccessService.Resolve*`, which reuses the id-then-path
rule `TenancyOf` already applies rather than making a second copy of it.

**78 of 239 mutating handlers were stripped. The other 161 were left, on
purpose**, and `TestNoMutatingHandlerChecksARole` holds the list:

- **The panel's loaders are shared with the reads.** `h.load`, `h.loadTile`
  and `h.settingsOrg` serve GET handlers as well, and the 192 GET routes carry
  no verb — for a read, the body check is still the only one there is.
  Stripping inside a shared loader opens the read. Gating reads is a separate
  job with its own capture, and it is not what point 18 was scoped to.
- **The rest sit in branches whose other arm does real work.** Removing them
  mechanically is not a swap. An attempted bulk pass ate the `d.OrgID`
  assignment out of `createDestination`'s else-branch — caught by the
  compiler, but it is the same class of edit that would silently drop a check
  somewhere the compiler could not see. Stopped there and did the rest by
  hand or not at all.

**Leaving them is safe in the direction that matters.** The route's gate runs
BEFORE the handler, so every one of these routes is now checked twice and none
is checked less than before. What they are not yet is ONE path — which is the
point of 18, and emptying that list is what finishes it.

**Two checks that are NOT levels and had to be carried across by hand:**

- Org setup readiness. `RequireOrgAccess` enforced it alongside membership,
  and the panel's `Gate` had no equivalent — without it, deleting the helper
  would let an org mid-wizard start accepting writes. `Gate` now calls
  `RequireOrgSetup` directly, as the API's gate calls `orgReady`.
- `KindDeferred` handlers keep their own org check, because the gate could not
  make one. `createDestination` is the clearest: server-wide needs an admin,
  an org's own needs write in that org, and which it is comes from the body.

**`ReadOnlyGuard` is still mounted site-wide and still reads the ACTIVE cookie
org.** On a gated route it is now a redundant second check against the wrong
org — harmless, because it is never stricter than `Gate`, but it is the
active-vs-resource confusion the capture froze a column to prevent. It stays
because the personal routes still need it. It should go when the reads are
gated.

**One test moved rather than broke.** `TestPreviewRequiresOrgWrite` called the
handler directly; with the check on the route that asserted nothing. It now
calls through `a.gate(...)`, which is the path the route uses.

## Point 18, step 3 — two holes found after the strip

**`POST /api/v1/resolve` lost its membership check and got it back.** The
handler took a linked `env_id` to enable relative paths, and the comment above
it says why it is checked: "only after confirming the caller may see it, or it
becomes a way to resolve names inside someone else's environment." The
loader swap turned `requireEnvAccess` into the bare `a.env`, and the route is
`KindDeferred`, so the middleware checked nothing either. Restored, with the
reason named at the call site.

That is the whole failure mode point 18 is dangerous for, and neither the
route-verb test nor the captured-level test could see it: the route declares a
verb, and the verb's level matches what was captured. Only reading the
KindDeferred handlers found it. **All five were audited**: `createStack`
(orgForCreate), `createDestination` (admin or requireOrgWrite on the body's
org), `createDomainResource` (resolveResourceTenancy, per level), the panel's
stack create (RequireOrgWrite on the active org), and resolve. The other four
were intact.

**`stillBodyGated` records the gate NAMES, not just that a gate exists.** As a
bare list of handler names it would pass a handler that swapped
`RequireStackAccess` (the resource's org) for `CanWrite` (the active cookie
org) — still "checks a role", same rung, different tenant. That is the
active-vs-resource confusion the captured table keeps a column for, and the
allowlist has to catch it too or it is the one artifact that can hide a
regression until the reads are gated. The test now compares the set and
reports drift separately from a missing or stale entry.

---

# The three decided changes — built 2026-09-19

The decisions and their reasoning are in 06-points-18-20.md. What is here is
what the build had to settle that the decision did not say.

## Decision 1, org domains → owner

One line in `handlers/web/server.go` and one edited row in
`routelevel_test.go`, which is the deliberate-change path the table's own
header describes. No new verb: `VerbOrgWrite` is already the owner-level org
verb and `.../domains/delete` already used it.

## Decision 3, deactivation revokes share links

`RevokeService.UserDeactivated` revokes by `CreatedBy` alone, with **no org
filter** — unlike `MembershipChanged`, which is scoped to the org whose role
changed. A deactivated account has no standing anywhere, so a per-org revoke
would leave its links in every other org open.

Called from the handler (`web/handler/settings.ToggleUserActive`) on the
disable edge only. Re-enabling does not un-revoke: a revoked link is a closed
exchange, and the row is kept as its audit line.

## Decision 2, API keys bind to their mint org

**Server admins mint unbound keys.** `CanGrantWrite` is
`CanWrite(c) || IsAdmin(c)` — an admin is offered write scopes on the strength
of the admin badge, and no org justified the grant. Binding their key to
whichever org the cookie happened to point at would narrow a credential on a
premise that was never true. `account.MintOrg` returns "" for an admin, and ""
for no active org (reachable during onboarding), both of which mean unbound.

**Enforced in two places, not one.** The route gate checks it inline, before
`access.Require`; `requireVerb` checks it for the writes that never reach a
gate, which is every `KindDeferred` handler resolving the org out of the body
(`requireOrgWrite` and `resolveResourceTenancy`, whose org and stack branches
both call it). The gate does NOT route through `requireVerb` — the two checks
are independent, so neither line is redundant. The remaining exception is
`orgForCreate` (POST /api/v1/stacks), which picks an org by role instead of
resolving one from the request and so reaches no gate at all; the binding
filters its candidate set. Those two cover every mutating route.

**Reads are deliberately not bound.** The decision says the *scopes* are
checked against the mint org. Narrowing `ctxOrgIDs` would have been one line
and would also have stopped a bound key *reading* another org — a tenancy
change, broader than what was decided, on the one decision with blast radius
outside the repo. Reads stay on live membership. If a bound key should be
blind to other orgs too, that is a second decision.

**404, not 403**, matching every other tenancy refusal on the API: a key that
may not act in an org does not learn the org exists.

**The binding is visible in two places** or it is a 404 with no explanation:
the key list at /account/apikeys names the org next to a bound key, and the
CLI approve page says which org the key it is about to mint will work in.

## Two artifacts the three changes touched, worth knowing about

**The captured-level table's comment column is parsed.** `gatefree_test.go`
reads handler names out of `# pkg.Func` comments in `capturedLevels`, because
`Route.Name` holds the verb now. Decision 1's row carries a note after the
name saying what raised it — which the parser was taking as part of the name,
silently dropping that handler from the gate-free scan with no failure. The
parser now takes the first field only. A row may carry provenance; the header
asks for it.

**Raising a route's level is not finished at the route.** Decision 1 made
`POST /orgs/:slug/settings/domains` owner-level, and the page rendering its
form still asked `CanWriteOrg` — so a member kept a form that now always
answers 403. `stackrmw.IsOwnerOf` is the owner-level twin of `CanWriteOrg`
(same active-vs-resource org distinction) and the page uses it. The delete
button on that page had the same mismatch before this change, since it was
already owner-level.

---

# Gating the reads — 2026-09-20

## The capture is not the walk

The AST walk that captured the writes is an upper bound for a read, because a
read-only handler's gate helper IS its body: resolve, refuse, render. The walk
says `getApp` reaches `requireOrgWrite` — it does, through the write branch of
a shared loader it calls with `false`.

So the walk was used to NARROW, not to decide: 192 GETs down to 22 reaching an
owner-level helper, and those 22 read by hand. Three things came out of that
reading and none of them is visible in the walk's output:

- **`RequireOrgWrite` is membership-only on a GET.** It refuses on
  `mutating(c)`, so every settings page that reaches it is read-level. Half the
  "write-looking" rows were this.
- **`ownedSettingsOrg` is the real owner gate on the panel**, and it answers
  404, not 403.
- **Four var-value GETs check `CanWriteOrg` explicitly**, with a comment saying
  why: `RequireOrgWrite` would not have refused them. Those are write-level
  reads and would have been silently downgraded by any rule read off the walk.

## Two things the gate already did, which is why reads could reuse it

`middleware.Gate` sets `CtxWriteHere` from the principal's level in the
resolved org, independently of the verb — so gating a read keeps
`CanWriteHere` honest and the buttons keep rendering correctly. And
`setupOpen` matches on the registered route pattern, so the wizard's own GETs
are exempt from the setup gate without a special case at the route.

## Why stillBodyGated does not reach zero

Because "every route names a verb" is not the same as "every check can move to
the route". A collection spans orgs; a KindDeferred route's org is in the body;
a move edits two orgs and only one of them is in the path. Those checks are
load-bearing, and the list says so next to each one. The test's own comment
says a new entry needs one of those reasons.

## The scanner's vocabulary shrank with the code

`CanWrite`, `CanWriteOrg`, `CanWriteHere` and `IsOwner` left `roleChecks`.
They are rendering flags now — mask a secret, hide a button — and a scanner
that cannot tell a flag from a gate reports every page that draws a disabled
button. What still catches a refusal built on one of them is the captured-level
tables, which say what each route enforces, plus ReadOnlyGuard underneath.

One scanner bug fixed on the way: a call like `h.tiles.Create(...)` was
followed as a call to THIS package's `Create`, because the local/imported test
only looked at a receiver that was a bare identifier. Four stack-create
handlers read as gated when they were not.

## Two things the exemption lists had to get honest about

`/avatars/*` was first written into the "public" list. It is not public: it
sits behind `auth.RequireAuth()` like any page. What it is, is not
org-addressed — the path is a storage key, not a resource id — so there is no
tenancy for a gate to resolve. Any signed-in user could fetch any avatar path
before the reads were gated and still can. It is on `ungatedReads` as "asset"
with that written down, because narrowing it is a decision about what an
avatar URL is, not part of moving authorization onto the route.

`resolveSettingsTarget`'s `write bool` became dead when the checks it guarded
moved to the four routes' verbs. Go does not complain about an unused
parameter, so nothing failed — and a parameter named `write` that used to mean
"authorize harder" and now means nothing is exactly what gets re-wired later
by someone who believes it still works. Deleted.

# Point 19, first domain slice — environments and variables

## A service read answers ErrNotFound, never (nil, nil)

`repo.Store` answers a missing row with `(nil, nil)`, so all forty handlers
that read an environment wrote the same two checks — the error, then the nil —
and a third of them got it subtly wrong: `if err != nil || env == nil { 404 }`
flattens a database failure into "not found", which is an outage that looks
like a typo.

`EnvironmentService.Get` and `.BySlug` answer `svcerr.ErrNotFound` instead.
The handler writes one check and hands the error to `middleware.HTTP`, which
has always mapped that to 404. A store failure now surfaces as the 500 it is.

The five callers that genuinely tolerate absence — the PR hook asking whether
a preview environment exists yet, the breadcrumb builder, the API's
`envByPath` resolver — say so with `errors.Is(err, svcerr.ErrNotFound)` rather
than by reading a nil. That is the difference this makes: "I expect this to be
missing sometimes" is now written down at the site that expects it.

No optional twin was added. Two methods for one read, one erroring and one
not, is the double vocabulary this whole point exists to remove.

## VariableService.List does not mask

`Masked` is not a property of the rows. It is a property of who is asking:
`canReadSecrets` in `handlers/api/v1/variables.go` decides it from the
principal's level in the resource's org, and the deploy path needs the
plaintext. A `List` that masked would either have to take the principal — a
service asking about authorization, which D-something says it must not — or
quietly blind the deployer. It returns the rows; masking stays at the surface
that knows the asker.

This is the first of the FILTERED reads the plan says to write down. The list
so far: `canReadSecrets`/`toVarEntries` (secret masking),
`a.dests.Visible(ctx, a.viewer(c))` (backup destinations), the per-row
`orgAllowed` in the API collections. An unfiltered forwarder for any of those
would silently widen what a page shows, which is why they move last and by
hand.

## Two nil services that only tests could find

`apiFor` built an `EnvironmentService` and never attached it; the journey
harness and four `project` tests built handlers with a bare `store`. Nothing
failed until a handler called the service, because a nil `*Service` is a
perfectly good field. Wiring, not logic — but it is the failure mode of every
remaining slice, so: a test that constructs a handler by hand must construct
the services it now depends on, and the scan that finds the reads does not
find the wiring.

# Point 19, third slice — organizations

## OrgService is not a read shell

The rule the plan sets is "never one forwarder per store method", and an
organization service built only of `Get`/`BySlug`/`ListAll` would have been
exactly that. Three things moved with the reads, and they are what justify the
type:

`StartDraft` — the whole body of the panel's `POST /orgs` except who may ask.
One draft per person, the placeholder name, the random slug, the creator's
owner row. The panel was the only surface that knew any of it.

`UnfinishedDraft` — two rules that were written as comments inside a private
panel helper: the newest draft wins (because `ListOrgsForUser` orders by name,
so "the first one" is a store-order accident once somebody owns two), and it
is matched on the caller's OWN role, not on "is an admin", or a stranger's
half-finished org becomes the one this admin's next answer to step 1 moves.

`Resolve` — slug first, id as fallback. There were three copies: the canvas
loader, the settings loader (each carrying a comment saying it had to match
the other), and the API's connector path, which had it written as a
slug-to-id translation rather than a load.

## ListAll and ListForUser are deliberately far apart

For stacks and tiles the unscoped listing is an admin convenience. For
organizations the org IS the tenancy unit, so reaching for `ListAll` where
`ListForUser` belongs does not widen a page, it crosses a tenant. The two are
named apart rather than being one method with a flag, and `ListAll`'s doc
comment names its three legitimate callers: the admin branch of the API's org
list, the server-wide setup-state map, and the "last organization cannot be
deleted" count. All four call sites were read before being given the method.

## MemberService.RoleOf next to AccessService.Principal

`Principal` already reads every membership row for the requesting user. A
second role lookup is the drift risk the whole extraction exists to remove, so
this one is deliberate and narrow: `Principal` answers "what can this REQUEST
do anywhere", building a map for the requester; `RoleOf` answers "what is this
ONE person to this ONE org", which is the question the invite page and the
member endpoints ask about somebody who is not the requester. The two share a
table read, not a rule, and neither can answer the other's question. Written
into `RoleOf`'s doc comment so the next reader does not collapse them.

`RoleOf` returns "" for a server admin, because an admin is not a member row.
Every caller already has the user, so none of them needed to be told twice.

## The net's regeneration can hide a new reader

`stillStoreReading` is regenerated from the scan each slice, which sets
`known := seen` and makes BOTH arms of the test pass trivially. A rewrite that
ADDED a store read to a previously clean function would be adopted silently
rather than failing.

So the regeneration is not the check. The check is a diff of the name column
against the previous commit's list, which must never gain a line:

	git show <prev>:...storefree_test.go | sed -n '/stillStoreReading/,/^`/p' \
	  | sed 's/ ->.*//' | sort > before
	# same over the working copy > after
	comm -13 before after   # must be empty

Run for all three slices so far. Empty each time; 525 -> 335 is real.

# Point 20

## Go 1.27 made the 258 rewrites go away

The plan doc priced option A at 258 composite-literal rewrites, because Go had
no flat literal for a promoted field: `repo.Tile{Name: "x"}` would have had to
become `repo.Tile{TileConfig: repo.TileConfig{Name: "x"}}`.

Go 1.27 allows promoted fields directly in a composite literal. `go.mod` moved
from `go 1.26.3` to `go 1.27.1` (and the two Dockerfiles' `ARG GO_VERSION`
with it; CI reads the version from `go.mod`), and **not one of the 258
literals changed**. The embedding is otherwise ordinary Go, nothing new.

That deletes the only argument for option B. The cost of A was the churn, and
there is no churn.

## No SetTileState — TileState is a read grouping only

The plan doc sketched `SetTileState(ctx, id, st TileState)` as the mirror of
`UpdateTile(ctx, id, cfg TileConfig)`. It is not built, on purpose.

A whole-struct state write is the bug this point exists to remove, pointed the
other way: a caller holding a `TileState` loaded a second ago, writing it back
to change `HomeNode`, would stomp a `Status` that changed in between. The five
narrow setters (`UpdateTileStatus`, `SetTileHomeNode`, `SetTileImageDigest`,
`SetTileLatestDigest`, `RecordTileRun`, plus `SetTileSharedNet`) each name one
fact and cannot express another. They were already correct; nothing replaced
them.

So the two halves are not symmetric. `TileConfig` is a write parameter.
`TileState` is a grouping that gives the observed fields one place to be
documented, and nothing takes one as an argument.

## The partition is defined by UpdateTile's SQL, not by taste

`TileConfig` holds exactly the columns the `UPDATE tiles SET ...` statement
names, minus `id` and `updated_at`. Checked mechanically, both directions
empty.

Those two are the trap. sqlx's named exec fails at RUN time, not compile time,
when the struct is missing a parameter the query names — and only in that
direction, extra fields are ignored. They are supplied by `tileConfigWrite`, a
bind struct local to `sqlite/tiles.go` that embeds `TileConfig` and adds the
two. Widening `TileConfig` to carry `ID` instead would put `db:"id"` at two
depths, which is the duplicated-field-list trap the plan rejected option C for.

Columns in neither half stay on `Tile` itself: `id`, `stack_id`,
`environment_id`, `slug`, `kind`, `webhook_token`, `created_at`, `updated_at`.
All identity or timestamps, all written only by `CreateTile` and `RenameTile`.

## The regression guard is reflective, not per-column

One mistake survives the type split: adding a field to `TileConfig` and
forgetting it in `UpdateTile`'s SQL. sqlx ignores the extra field, so the
column silently stops persisting — the same class of quiet failure, moved.

`sqlite/tileconfig_test.go` fills every `TileConfig` field reflectively with a
distinct non-zero value, writes, reads back and compares field by field, so a
field added later is covered without anyone remembering to cover it. Verified
to have teeth: dropping `node_group = :node_group` from the SQL fails it by
name. The typed per-column cases in `newcols_test.go` stay; they document
which columns arrived when.

## UpdateTile no longer stamps the caller's struct

It used to set `t.UpdatedAt` on the caller's `*repo.Tile` as a side effect of
writing the row. With a value parameter it cannot, and no caller read it back —
every one of them re-loads or discards the struct. Worth knowing before someone
looks for a stale `UpdatedAt` in a rendered page.
