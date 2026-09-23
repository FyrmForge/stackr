# Sweep D — API v1, middleware, stream

Every non-generated `.go` file in `internal/stackrd/handlers/api/v1`,
`handlers/api`, `handlers/api/handler/health`, `handlers/middleware` and
`handlers/stream`, read top to bottom.

Files read: 56 files, 10 284 lines.

| File | Lines |
|---|---|
| api/handler/health/handler.go | 37 |
| api/server.go | 171 |
| api/v1/admin_test.go | 48 |
| api/v1/apps.go | 421 |
| api/v1/auth.go | 543 |
| api/v1/auth_test.go | 87 |
| api/v1/backups.go | 350 |
| api/v1/backups_test.go | 217 |
| api/v1/config.go | 278 |
| api/v1/config_test.go | 274 |
| api/v1/cron_test.go | 59 |
| api/v1/databases.go | 286 |
| api/v1/databases_test.go | 101 |
| api/v1/domainresources.go | 195 |
| api/v1/domainresources_test.go | 82 |
| api/v1/domains.go | 85 |
| api/v1/envs.go | 141 |
| api/v1/errors.go | 64 |
| api/v1/forward.go | 201 |
| api/v1/guard_test.go | 94 |
| api/v1/helpers.go | 195 |
| api/v1/lifecycle.go | 312 |
| api/v1/logs.go | 118 |
| api/v1/members.go | 184 |
| api/v1/orgconfig.go | 158 |
| api/v1/patch_test.go | 211 |
| api/v1/proxycfg.go | 78 |
| api/v1/registry.go | 289 |
| api/v1/releases.go | 152 |
| api/v1/resolve.go | 211 |
| api/v1/settings.go | 306 |
| api/v1/settings_test.go | 96 |
| api/v1/slugpath.go | 151 |
| api/v1/slugpath_test.go | 176 |
| api/v1/stacks.go | 92 |
| api/v1/storage.go | 205 |
| api/v1/types.go | 803 |
| api/v1/v1.go | 559 |
| api/v1/v1_test.go | 161 |
| api/v1/variables.go | 369 |
| api/v1/variables_test.go | 271 |
| api/v1/volumes.go | 82 |
| api/v1/volumes_test.go | 87 |
| middleware/actor.go | 23 |
| middleware/audit.go | 58 |
| middleware/logging.go | 70 |
| middleware/managed.go | 34 |
| middleware/managed_test.go | 39 |
| middleware/orgctx.go | 476 |
| middleware/orgctx_test.go | 73 |
| middleware/readonly_test.go | 54 |
| middleware/setupgate.go | 118 |
| middleware/setupgate_test.go | 98 |
| middleware/svcerr.go | 71 |
| middleware/tenancy_test.go | 88 |
| middleware/theme.go | 39 |
| stream/hub.go | 43 |

Confirmed from the pre-sweep list, not re-derived: `v1/storage.go:104` (ServerID
defaulting to `"local"`), `v1/storage.go:175` (consumer scan), `v1/storage.go:184`
(sub-path volume cleanup), `v1/storage.go:196` (`orgShareOwner` config-managed
refusal), `v1/lifecycle.go:231` (third copy of the storage probe),
`v1/backups.go:204-208` (`Remove` + manual `ReloadBackups` where `Delete`
already reloads).

---

## Findings

### Bugs — a rule spelled twice, and the two spellings have drifted

#### v1/members.go:117-132,152-167,178-184 — invite minting bypasses MemberService
- **Rule:** role whitelist, expiry bounds, already-a-member check, invite row + mail.
- **Category:** 6 (duplication across surfaces) + 2 (validation).
- **Owner it should have:** `service.MemberService.Invite` (`service/member.go:85-102`).
- **Duplicated at:** `v1/members.go:45-63` (`addMember`) and
  `web/handler/org/members.go:246-250` (`newInvite`) both call
  `members.Invite`. `createInvite` is the odd one out: it defaults the expiry
  to 7 days with **no upper bound**, skips the already-a-member check, and
  mints the row through the raw escape hatch `members.CreateInvite`
  (`service/member.go:241-246`) instead of `Invite`.
- **Severity:** **bug.** `slugpath_test.go:TestUnknownRoleIsRefused` asserts
  that an unbounded (and a negative) expiry must be refused —
  `POST /api/v1/orgs/{id}/invites` accepts both. Two routes on one surface with
  different guarantees.
- **Separately, a shared defect, not a divergence:** both surfaces fold an
  unknown role to `"member"` *before* the service can refuse it —
  `v1/members.go:178-184` (`validRole`) and
  `web/handler/org/members.go:314-316`. The same test says folding is the wrong
  answer; the service's refusal never fires from either surface.

#### v1/backups.go:321-349 — `patchDestination` re-implements `SetShared`
- **Rule:** a server-wide destination may only be unshared while nothing
  schedules against it; unshare then persist.
- **Category:** 6 + 3.
- **Owner it should have:** `service.BackupDestinationService.SetShared`
  (`service/backupdestination.go:156-170`), which already owns the `Global()`
  guard, the `Users()` conflict and the write.
- **Duplicated at:** `web/handler/settings/handler.go:662-682`
  (`ToggleDestinationShared`) calls `dests.SetShared`. The API inlines the
  `Users()` scan at `:337-342`, assigns `d.Shared` at `:344` and writes through
  the raw `dests.Save` at `:346`.
- **Severity:** **bug.** Same escape-hatch pattern already flagged at
  `v1/backups.go:204-208`, and the refusal text differs: API `"still used by X"`,
  service `"still used by X; move those schedules first"`.

#### v1/registry.go:216-227 — `createRegistry` re-implements `AddExternal`
- **Rule:** name and url are required and trimmed; mint id and CreatedAt.
- **Category:** 2 + 6.
- **Owner it should have:** `service.RegistryService.AddExternal`
  (`service/registry.go:42-51`).
- **Duplicated at:** `web/handler/settings/handler.go:624-631`
  (`CreateRegistry`) calls `AddExternal`. The API validates in the handler
  (`:218`), builds `repo.Registry` itself (`:221-223`) and calls the raw
  `registries.Create`.
- **Severity:** **bug** (web-vs-API divergence on the same write).

#### v1/registry.go:275-279 — managed-registry delete refusal spelled twice
- **Rule:** the managed registry cannot be removed.
- **Category:** 1 + 6.
- **Owner it should have:** `service.RegistryService.Delete`
  (`service/registry.go:54-65`), which already refuses it.
- **Duplicated at:** `web/handler/settings/handler.go:634-641` delegates to the
  service; a comment there says the handler used to do it by hand and that was
  a bug. The API still does it by hand.
- **Severity:** **bug** (the exact pattern the panel was fixed out of).

#### v1/registry.go:233-267 vs web/handler/settings/handler.go:604-621 — two shapes for "set the registry domain"
- **Rule:** setting a TLS domain on the managed registry must go through
  `px.SetRegistryDomain` (which also runs `registry.EnsureManaged`); an absent
  managed registry is not an error the caller can act on.
- **Category:** 6.
- **Owner it should have:** `RegistryService` / `proxy.Service`.
- **Duplicated at:** the panel resolves via `registries.Managed()` and answers
  400 `"enable the managed registry first"` on `ErrUnavailable`; the API loads a
  registry **by id**, branches on `r.Managed` (`:253`) and for a non-managed row
  writes `r.Domain` and `registries.Save` directly (`:261-264`) — a path the
  panel has no equivalent for at all.
- **Severity:** **bug.**

#### v1/releases.go:35-120 — second implementation of the release/promotable set
- **Rule:** which commits are built, which are building, which env runs which
  commit, and which pending config plan blocks a promote.
- **Category:** 1 + 3 + 6.
- **Owner it should have:** `service.ReleaseService` (it already owns `Target`
  and `Promote`; the *listing* has no owner).
- **Duplicated at:** `web/handler/project/releases.go:105-135` (`releaseView`)
  + `web/handler/project/commitlog.go:438-490`. The two disagree:
  - pending plan — API `releases.go:110-115` matches any pending plan whose
    `CommitSHA` equals the commit, ignoring `EnvSlug`; panel
    `releases.go:88-104` (`planWaiting`) counts only a plan **at or before** the
    commit, treats a stack-scoped row (`EnvSlug == ""`) as covering every env,
    and excludes a plan on a newer commit.
  - "runs this commit" — API takes the newest `done` deployment over
    `Type == "static"` envs and `SourceType == "git"` tiles only
    (`releases.go:51-97`); the panel reads `log.Runs[env.ID]` off the commit log.
  - "building" — API `deploystate.IsLive(d.Status)`; panel keys off render
    chips `ch.State == "building" || "deploying"` (`releases.go:117-121`).
- **Severity:** **bug.** A CI gate reading `pending_plan` off the API gets a
  different answer than a person looking at the panel's ladder.

#### v1/errors.go:15-30 vs middleware/managed.go:14-34 — the config-managed refusal, two implementations, two messages
- **Rule:** a structural write on a config-managed stack is a 409.
- **Category:** 1 + 6.
- **Owner it should have:** one service guard (`GateService` already carries
  the staging half of this).
- **Duplicated at:** API `managedGuard`/`rejectManaged` says
  `"stack is managed by its config file; edit the file to change its structure"`;
  panel `ManagedErr`/`RequireUnmanaged` says
  `"stack is managed by <ConfigRepo>; edit the config file to change its structure"`.
  The panel's names the repo, the API's cannot. Call sites: `v1/variables.go:111`,
  `v1/volumes.go:72`, `v1/lifecycle.go:192`, `v1/domainresources.go:153`.
- **Severity:** **bug** (drift in the refusal a user is meant to act on).

#### v1/auth.go:300-306 vs middleware/setupgate.go:113-118 — the unfinished-wizard gate, twice
- **Rule:** an org whose onboarding wizard is open answers nothing but its own
  wizard.
- **Category:** 1 + 6.
- **Owner it should have:** one predicate; the surfaces may render it
  differently, the *decision* should not be written twice.
- **Duplicated at:** API `orgReady` returns a 409 whose text embeds a panel URL
  (`/orgs/<slug>/setup/done`); panel `RequireOrgSetup` returns a typed
  `SetupPending` and additionally honours the `setupOpen` route allowlist
  (`setupgate.go:96-107`), which the API has no equivalent of. Called from
  `v1/auth.go:335,479`, `v1/helpers.go:77,95`, `v1/variables.go:274`.
- **Severity:** **bug** (one rule, two truth tables — the API cannot express
  the setup-open exemption).

#### v1/auth.go:442-491 vs middleware/orgctx.go:387-476 — point 18's "one authorization path", implemented twice
- **Rule:** resolve principal → resolve tenancy for the route's Kind →
  `access.Require(verb, org)`, with the `ErrServerOwned` admin branch, the
  wizard branch, and the `CtxWriteHere` record.
- **Category:** 6, security-relevant.
- **Owner it should have:** `service.AccessService` (a gate the two surfaces
  parameterise, not two copies).
- **Duplicated at:** both files carry the same four-branch switch. They already
  differ deliberately (panel 404s `VerbOrgOwnerRead`/`VerbAdminRead` level
  refusals, `orgctx.go:456-461`; API always 403s) — which is exactly how the
  *next* difference will arrive unannounced.
- **Severity:** **bug.**

#### v1/settings.go:192-255 — the knob catalogue, and the panel's four hand-written ones
- **Rule:** which defaults exist at which level, and how each resolves and reads
  back.
- **Category:** 6.
- **Owner it should have:** `config/settings` (it already owns `Settings`,
  `Parse`, `Resolve`, `Levels`) — one catalogue both surfaces enumerate.
- **Duplicated at:** `settingKeys` is unexported in package `v1`, so it is
  API-only; the panel writes its field list out by hand at four places, and the
  five lists do not agree on which knobs exist at which level:
  - `v1.go:367-374` mounts the same 14-knob catalogue at server, org, stack
    **and** env;
  - `web/handler/server/server.templ:425-468` — 10 knobs (server);
  - `web/handler/org/defaults.templ:24-36` — 4 (`cron_timeout_min`,
    `cpu_limit`, `mem_limit_mb`, `run_retention_days`);
  - `web/handler/project/stacksettings.templ:79-87` — 3 (stack);
  - `web/handler/project/stacksettings.templ:508-516` — 3 (env).

  So `PATCH /api/v1/stacks/{id}/settings` publishes and accepts
  `build_node`, `node_group`, `metric_retention_hours`, `protect_password` and
  the four concurrency knobs at stack and env level, where the panel offers
  none of them.
- **Severity:** **bug** (five lists, different per level; a knob added to
  `config/settings` has to be written into all five by hand).

---

### Authorization and scope rules living in handlers (security-relevant)

#### v1/helpers.go:29-100 — `orgForCreate`
- **Rule:** which org a new stack lands in — admin sees all, else
  owner/member-role orgs; bound keys are filtered out; orgs mid-wizard are
  dropped from the candidate set but produce a 409 when named explicitly;
  0 candidates → 403, >1 → 400.
- **Category:** 1 + 3, authorization.
- **Owner it should have:** `OrgService` / `AccessService`.
- **Duplicated at:** the role test at `:39` re-spells `LevelWrite` by string
  comparison (`role == "owner" || role == "member"`), the same predicate as
  `middleware.CanWriteOrg` (`orgctx.go:209-219`) and
  `service.AccessService.Require`. The comment at `:44-46` states outright that
  this is "the one write that never reaches requireVerb".
- **Severity:** high.

#### v1/domainresources.go:53-97 — `resolveResourceTenancy`
- **Rule:** instance-level domain resources are admin-only; org/stack levels
  need membership plus write in that owner's org; `"node"` is an alias for
  `"instance"`; a missing owner is a 400.
- **Category:** authorization + 2.
- **Owner it should have:** `DomainResourceService` behind an `AccessService`
  verb (the route already declares `VerbDomainWrite`/`KindDeferred`, so this
  handler check is the only one that runs).
- **Duplicated at:** the `"node"`→`"instance"` alias is written twice
  (`:56` and `:106-108`); called again from `deleteDomainResource:135` and
  `patchDomainResource:178`.
- **Severity:** high.

#### v1/variables.go:39-49 — `canReadSecrets`
- **Rule:** revealing a secret value needs both the `secrets:read` scope and a
  live write-level role in the resource's own org.
- **Category:** authorization.
- **Owner it should have:** `VariableService` / `AccessService`.
- **Duplicated at:** the panel spells the same rule as a route verb plus
  `middleware.AuditServeValue` (`middleware/audit.go:49-58`). Five call sites
  here: `:96, :125, :159, :188, :237, :289`.
- **Severity:** high (correct today; it is the *placement* that is the risk —
  `variables_test.go:TestSecretsReadStillNeedsWriteInTheOrg` exists because
  this already regressed once).

#### v1/auth.go:356-389 — `requireVerb` / `keyOrgAllows`
- **Rule:** a key minted in org A may not act in org B, and the live role in the
  target org bounds the key's write scopes.
- **Category:** authorization.
- **Owner it should have:** `AccessService` (the key binding is a property of
  the principal, not of a handler helper).
- **Duplicated at:** called from the handler body in `backups.go:58`,
  `domainresources.go:78,90`, and again from inside `v1/auth.go:483` (`gate`)
  and `helpers.go:48` (`orgForCreate`).
- **Severity:** high.

#### v1/auth.go:248-257 — `adminOnly`
- **Rule:** storage, proxy-config and registry-admin routes are server-admin
  only.
- **Category:** authorization.
- **Owner it should have:** the verb table — these routes already declare
  `service.VerbAdminRead`/`VerbNodeManage`/`VerbProxyAdmin`
  (`v1.go:380-455`), so `adminOnly` is a second vocabulary wrapped on top of
  the first on 11 routes.
- **Severity:** medium-high (redundant today; two places to change).

#### v1/backups.go:54-60,77-85 — destination create/delete authorization
- **Rule:** only an admin may create or remove a server-wide destination; an
  org destination needs write in that org; someone else's org gets 404, not 403.
- **Category:** authorization.
- **Owner it should have:** `BackupDestinationService`.
- **Duplicated at:** the panel's `destOrg` + route verbs
  (`web/handler/settings/handler.go:307-352`).
- **Severity:** high (`KindDeferred` means the route gate checks nothing, so
  this handler check is the only one).

#### api/server.go:150-171 — `nodeSamples`
- **Rule:** the agent sample push is authenticated by a bearer token compared
  against `agent.ReadKey(dataDir)`.
- **Category:** authentication, in a route closure in the router file.
- **Owner it should have:** `nodes.Service` or an agent middleware.
- **Duplicated at:** nowhere — which is the problem: it is the only
  authentication path in the codebase that is not `KeyAuth` or `BrowserAuth`.
- **Severity:** high.

#### middleware/orgctx.go:289-320 — `ReadOnlyGuard`
- **Rule:** viewers may not mutate, except `/logout`, `/orgs/switch`, `/orgs`,
  and anything under `/account/` or `/notifications/`.
- **Category:** authorization by URL string.
- **Owner it should have:** the verb table (a route that needs no org right
  should say so, not be listed in a prefix allowlist).
- **Severity:** high (a new self-service route under a different prefix is
  silently refused for viewers; a new org-content route under `/account/` is
  silently allowed).

#### middleware/orgctx.go:151-285 — the second role vocabulary
- **Rule:** `CanWrite`, `CanWriteOrg`, `IsOwner`, `IsOwnerOf`, `InOrg`,
  `RequireOrgAccess`, `RequireOrgWrite`, `RequireStackAccess` — owner/member/
  viewer compared by string.
- **Category:** authorization.
- **Owner it should have:** `AccessService` levels (`LevelWrite` etc.), which
  `Gate` in the same file already uses at `:469-471`.
- **Severity:** high (two vocabularies for the same question, in one file).

#### middleware/setupgate.go:96-107 — `setupOpenRoutes`
- **Rule:** three route patterns stay reachable while an org's wizard is open.
- **Category:** authorization allowlist.
- **Owner it should have:** the route declaration, not a map in middleware.
- **Severity:** medium (security-adjacent; the API has no equivalent — see the
  `orgReady` bug above).

---

### Direct infra / raw-store use from a handler (rubric 4)

| Location | What it reaches for | Should own it | Severity |
|---|---|---|---|
| `v1/forward.go:50,60,64` | `rt.RunningTasks`, `envnet.ServiceFor(ctx, a.store, t)`, `envnet.Net(ctx, a.store, …)`, `rt.DialOnNetwork` | a `ForwardService`; none exists | high |
| `v1/logs.go:31,48,59,68` | `clus.ServiceLogs`, `clus.Logs`, `clus.StreamServiceLogsMarked`, `clus.StreamLogsMarked`, `clus.Self` | `TileTelemetryService` | high |
| `v1/lifecycle.go:231-235` | `clus.NodeOfStorage`, `storagetiles.Probe`, `clus.RemoveVolume`, then writes `st.Status`/`st.StatusMsg` and `storage.Save` | `StorageService.Probe` (established finding; note it also *assigns the status* here) | high |
| `v1/lifecycle.go:75` | `engine.EnqueueRollback` | `DeployService` has `Trigger` and `Cancel` but no `Rollback` | high |
| `v1/storage.go:184-185` | `clus.NodeOfStorage`, `clus.RemoveVolume` | `StorageService` (established) | high |
| `v1/config.go:105,150` | `applier.Planner.RunAll`, `applier.Planner.PreviewBundle` | `PlanService` | high |
| `v1/orgconfig.go:34,69,139` | `orgcfg.Plan`, `orgcfg.PreviewBundle`, `orgconf.EnqueueApply(ctx, a.work, …)` | `PlanService` / an org-config service | high |
| `v1/config.go:259,273` | `stackconf.ExportStack(ctx, a.store, s)`, `orgconf.ExportYAML(ctx, a.store, o)` | a service; both take the raw store from a handler | medium |
| `v1/variables.go:165,321` | `varref.New(a.store).Resolve`, `varref.Catalogue(ctx, a.store, …)` | `VariableService` | high |
| `v1/variables.go:65,163` | `audit.Record(ctx, a.store, …)` | `AuditService` (it exists — `api/server.go:72` wires one the API never receives) | medium |
| `v1/backups.go:166,168,172` | `backup.ParseRef`, `backup.ResolveNamed(ctx, a.store, …)`, `backup.ResolveDestination(ctx, a.store, …)` | `BackupDestinationService` — **and this function is dead**, see below | low |
| `v1/backups.go:249,278,298` | `backups.Start`, `backups.StartRestore`, `backups.LatestRestore` | `BackupScheduleService` | medium |
| `v1/databases.go:199` | `managedtiles.ResolveTarget(ctx, a.store, …)` | `ManagedInstanceService` | medium |
| `v1/databases.go:64,166` | `managedtiles.InfraPath` | `ManagedInstanceService` | low |
| `v1/resolve.go:41` | `managedtiles.ResolveTarget(ctx, a.store, …)` | as above | medium |
| `v1/resolve.go:181` | `managedtiles.ResourceSlug` | as above | low |
| `v1/storage.go:178` | `storagetiles.ParseAttachment` | `StorageService` (established) | high |
| `v1/lifecycle.go:285` | `repo.LoadPRConfig(ctx, a.store, s.ID)` | `PREnvService` — the write side already goes through it at `:303` | medium |
| `v1/settings.go:76-82` | `settings.Levels(ctx, a.store, …)` | `SettingsService` | medium |
| `v1/envs.go:136-141` | builds a whole `envops.Ops` (cluster runtime, proxy, a fresh `managedtiles.NewService`) | **dead code**, see below | medium |
| `v1/registry.go:99,145,190` | `registry.NewClient(reg, a.signer)` + `.Images/.Tags/.Tag/.DeleteTag` | `RegistryService`; `web/handler/org/registry.go:78,221` does the same | medium |
| `v1/volumes.go:22-26` | `tiles.ListAll` then `repo.VolumesAttachedTo` — whole-table scan per request | `TileService` | medium |
| `v1/apps.go:19`, `databases.go:16`, `storage.go:175` | `tiles.ListAll` + in-handler filter | the services | medium |
| `middleware/orgctx.go:73-75,111,217,235,280,358-361,431` | `store.ListOrgs`, `ListOrgsForUser`, `GetOrgMember`, `GetStack`, `GetOrgBySlug`, `GetOrg` | `OrgService` / `AccessService` | high |
| `middleware/setupgate.go:37-54` | `store.GetOrgBySlug`, `GetOrg`, `GetOrgMember` | `OrgService` | medium |
| `middleware/managed.go:26` | `store.GetStack` | `StackService` | medium |
| `middleware/audit.go:38,41,53` | `audit.Record(ctx, s, …)` with a raw `repo.Store` | `AuditService` | medium |

---

### Domain rules and orchestration with no owner

#### v1/apps.go:34, stacks.go:19, databases.go:27, domainresources.go:36-43 — five spellings of "filter a listing to the caller"
- **Rule:** a collection endpoint returns only rows in orgs the caller may see,
  with the unfinished-wizard filter applied.
- **Category:** 1 + 3 + 6.
- **Owner it should have:** a service taking `service.Viewer` — the shape
  already exists and is already used by `listDestinations`
  (`v1/backups.go:30` → `dests.Visible(ctx, a.viewer(c))`, with `viewer` built
  at `v1/auth.go:236-246`). Every other listing does a `ListAll` and then
  hand-rolls `!a.orgAllowed(c, …)` per row, `listDomainResources` with a
  level-dependent switch of its own.
- **Severity:** high (one question, five implementations on one surface, with
  the target already built and in use next door).

#### v1/logs.go:26-52 — log source fallback
- **Rule:** service logs, else re-resolve to a container, else empty.
- **Category:** 3 + 6.
- **Owner it should have:** `TileTelemetryService` (it owns `Logs` and
  `Container`; it does not own the fallback between them).
- **Duplicated at:** `web/handler/app/handler.go:153-166` calls the same
  `telemetry.Logs` but has a plain if/else with no `telemetry.Container`
  re-resolve. The API is the more thorough of the two and the two are not the
  same operation (JSON read vs SSE stream), so this is duplication rather than
  drift.
- **Severity:** medium.

#### v1/helpers.go:103-117 — `resolveEnv`
- **Rule:** an app/database created without an `env_slug` lands in the stack's
  **first** environment; a stack with no environments is a 400.
- **Category:** 1.
- **Owner it should have:** `EnvironmentService`.
- **Duplicated at:** called from `apps.go:63` and `databases.go:153`.
- **Severity:** medium.

#### v1/helpers.go:190-195 — `stackChanged`
- **Rule:** nudge the open canvases after a non-runtime change in a stack.
- **Category:** 5 (side effect fired by the caller).
- **Owner it should have:** the services that perform the write.
- **Duplicated at:** `variables.go:209,259`; the comment admits the panel
  nudged and the API did not. `forward.go:114-119` is a third hand-rolled nudge.
- **Severity:** medium.

#### v1/apps.go:67-73 — kind defaulting and validation in `createApp`
- **Rule:** an absent `kind` is `"service"`; a kind must be in `runpolicy` or be
  the special-cased `"volume"`.
- **Category:** 2.
- **Owner it should have:** `TileService.Create` (which owns every other
  creation rule, per the comment at `:83-84`).
- **Severity:** medium.

#### v1/apps.go:122-292 — `patchApp`'s present-key merge
- **Rule:** 40 fields merged by "was the key present in the raw body".
- **Category:** mostly wire-shape (D6, legitimately at the edge), except
  `:226-228`: clearing `basic_auth_user` also clears the password. That is a
  domain rule inside the merge.
- **Owner it should have:** the credential-pair rule belongs in `TileService`.
- **Severity:** medium.

#### v1/databases.go:180-221, 226-251 — `provisionApp` / `attachProvision`
- **Rule:** only services and crons may consume a slice; an `infra_path` must
  name an instance and not a slice; then Provision/Attach **and** Wire, in that
  order, as two service calls with no transaction between them.
- **Category:** 1 + 3.
- **Owner it should have:** `SliceService` (a single `Provision`/`Attach` that
  wires).
- **Duplicated at:** the "only services and crons" test is written twice
  (`:185` and `:231`); the panel's drawer has its own path
  (`web/handler/db/handler.go`).
- **Severity:** high (a failure between Provision and Wire leaves an unwired
  slice).

#### v1/databases.go:39-65, 67-77 — `infraPath` / `toDBOut`
- **Rule:** `ScopeKind == ""` means `"env"`.
- **Category:** 1.
- **Owner it should have:** `repo.Tile` or `ManagedInstanceService`.
- **Duplicated at:** written at `:57` and again at `:69`.
- **Severity:** low.

#### v1/lifecycle.go:120-144 — `createAutoDomain`
- **Rule:** after `domains.AddAuto`, re-list the tile's domains and find one
  flagged `Auto`; its absence means "no domain resource to nest under".
- **Category:** 3 — the rule is inferred from an empty result instead of
  reported by the service.
- **Owner it should have:** `DomainService.AddAuto` should say so.
- **Severity:** medium.

#### v1/lifecycle.go:169-172, v1/domains.go:56-63, v1/domains.go:80-83, v1/lifecycle.go — the `staged` → 409 wording
- **Rule:** a stack that stages edits refuses API field edits and points at the
  canvas.
- **Category:** 6.
- **Owner it should have:** one error from `GateService`.
- **Duplicated at:** three near-identical messages differing only in the verb
  ("change"/"add"/"remove" the domain).
- **Severity:** low.

#### v1/volumes.go:63-82 — `deleteVolume`
- **Rule:** `IsVolume` check, managed-stack refusal, teardown, then
  `deploys.RedeployIfRunning(attached, "volume")`.
- **Category:** 3 + 5.
- **Owner it should have:** `TileService.Delete` (the redeploy is the owner's
  side effect, not the caller's).
- **Duplicated at:** `web/handler/server/handler.go:270-320` is the panel's
  volume path (established: no `VolumeService` exists).
- **Severity:** medium.

#### v1/orgconfig.go:131-133, 150-152 — `cp.Status != "pending"` → 409, twice
- **Rule:** a decided plan cannot be re-decided.
- **Category:** 1 + 6.
- **Owner it should have:** `PlanService` — which already owns it on the
  **stack** side (`approvePlan`/`rejectPlan` in `config.go:169-196` delegate and
  carry no status check, and `config_test.go:TestDecideRejectsNonPendingPlan`
  proves the service enforces it). The org side re-spells it in the handler.
- **Severity:** medium (asymmetry inside one surface).

#### v1/orgconfig.go:58-60 / v1/config.go:44-52 — "not bound to a config repo"
- **Rule:** a stack/org with no config binding has nothing to plan.
- **Category:** 1 + 6.
- **Owner it should have:** `PlanService`.
- **Duplicated at:** `config.go:50` (`requireConfigStack`), `config.go:110`
  and `config.go:153` (from `stackconf.ErrNotBound`), `orgconfig.go:39`,
  `orgconfig.go:59`, `orgconfig.go:72` — six spellings of the same 409 across
  two files.
- **Severity:** medium.

#### v1/config.go:228-245 — `toPlanDetail`'s `ch.Declared()` swap
- **Rule:** a change row for a value the config declares carries `Scope`, not
  `Env`.
- **Category:** 1 (domain rendering rule).
- **Owner it should have:** `stackconf.Change`.
- **Severity:** low.

#### v1/forward.go:184-201 — `forwardPort`
- **Rule:** explicit port → the engine's default for a managed tile → the
  tile's declared port → 400.
- **Category:** 1 + 2.
- **Owner it should have:** a forward service / `managedtiles`.
- **Severity:** medium.

#### v1/resolve.go:101-112 — slice env and slug defaulting
- **Rule:** a slice lands in the caller's named env, else the instance's own;
  an absent slug is the name.
- **Category:** 1.
- **Owner it should have:** `SliceService.Cut`.
- **Severity:** medium.

#### v1/settings.go:102-146, 260-283 — `patchSettingsFor`, `ownAndSource`
- **Rule:** JSON `null` clears an override back to inherit; the "source" of a
  value is the deepest level that sets it.
- **Category:** 1.
- **Owner it should have:** `config/settings` + `SettingsService`.
- **Severity:** medium.

#### v1/slugpath.go:47-124 — the colon-path addressing grammar
- **Rule:** `org:stack:env:tile`, colon or slash, no bare slugs below org level.
- **Category:** 2.
- **Owner it should have:** `AccessService.Resolve*` or beside
  `managedtiles.ResolveTarget`, which is a second addressing grammar for the
  same paths (`resolve.go:41`, `databases.go:199`).
- **Severity:** medium.

#### v1/auth.go:65-167 — the capability catalogue
- **Rule:** which scopes exist, which require content-write to grant, and
  `GrantableScopes`' filter.
- **Category:** 1 + 2.
- **Owner it should have:** a service — the panel's account page and both
  halves of the CLI login import this from the HTTP handler package (per the
  comment at `:156-157`).
- **Severity:** medium.

#### middleware/orgctx.go:61-120 — `OrgContext`'s active-org policy
- **Rule:** prefer a finished org over a draft; a cookie naming a draft loses
  to a finished org; admins act as `"owner"` everywhere.
- **Category:** 1.
- **Owner it should have:** `OrgService` (it is org policy, not request
  plumbing).
- **Severity:** medium.

#### middleware/setupgate.go:29-73 — `RequireSetupDone`'s three-way branch
- **Rule:** owner → redirect to the wizard summary; member → `SetupPending`;
  neither → 404.
- **Category:** 1.
- **Owner it should have:** `OrgService`.
- **Severity:** medium.

---

### Dead code carrying rules nothing runs

These compile, read as enforcement, and enforce nothing. Each is a rule that
moved into a service and left its old copy behind — the next reader cannot tell
which one is live.

| Location | What it claims to do | Where the rule actually lives now |
|---|---|---|
| `v1/helpers.go:124-136` `checkConnector` | a connector must belong to the stack's org | `TileService` (proved by `patch_test.go:TestPatchAppRejectsForeignConnector`) |
| `v1/domainresources.go:146-164` `rejectManagedOwner` | a config-managed owner's `domains:` is file-owned | `DomainResourceService` |
| `v1/backups.go:164-176` `resolveDestinationRef` | `${{ org.backups.NAME }}` resolution | `BackupScheduleService` |
| `v1/envs.go:39-52` `envRunning` | "is anything deployed here" | `EnvironmentService.Delete/Reset` |
| `v1/envs.go:136-141` `envOps` | builds an `envops.Ops` with cluster, proxy and a fresh managed-tiles service | nothing calls it |
| `v1/auth.go:409-430` `requireTile` | tile tenancy + write role | the route gate; only tests call it |
| `v1/registry.go:26-36` | `write bool` parameter, never read | — |
| `v1/variables.go:139-147` | `tile *repo.Tile` parameter, never read | — |

**Severity:** medium (each is a misleading second copy of a live rule).

---

## Web-vs-API divergences

| Rule | Web file:line | API file:line | How they differ |
|---|---|---|---|
| Create an invite | `web/handler/org/members.go:246-250` → `members.Invite` | `v1/members.go:117-132,152-167` → `members.CreateInvite` | API applies no expiry upper bound and skips the already-a-member check; the panel gets both from the service. Role folding is *not* a divergence — both surfaces fold before the service sees it (`v1/members.go:178-184`, `web/handler/org/members.go:314-316`). |
| Share / unshare a server-wide destination | `web/handler/settings/handler.go:662-682` → `dests.SetShared` | `v1/backups.go:321-349` | API inlines the `Users()` conflict and writes through raw `dests.Save`; refusal text differs. |
| Add an external registry | `web/handler/settings/handler.go:624-631` → `registries.AddExternal` | `v1/registry.go:216-227` | API validates and builds the row in the handler, then calls raw `registries.Create`. |
| Delete a registry | `web/handler/settings/handler.go:634-641` → `registries.Delete` | `v1/registry.go:269-284` | API re-spells the managed-registry refusal in the handler — the exact bug the panel was fixed out of. |
| Set the managed registry's TLS domain | `web/handler/settings/handler.go:604-621` | `v1/registry.go:233-267` | Panel resolves via `registries.Managed()` and 400s when unavailable; API addresses by id, branches on `r.Managed`, and for a non-managed row writes `Domain` + `Save` directly. |
| Release / promotable set | `web/handler/project/releases.go:105-135`, `commitlog.go:438-490` | `v1/releases.go:35-120` | Different "pending plan blocks this promote" rule (env-scope and commit-ordering aware on the panel, exact-SHA only on the API), different "runs this commit" and "building" derivations. |
| Config-managed structural write refusal | `middleware/managed.go:14-34` | `v1/errors.go:15-30` | Two implementations; the panel's message names the config repo, the API's cannot. |
| Unfinished-org gate | `middleware/setupgate.go:113-118` (+ `setupOpen` allowlist `:96-107`) | `v1/auth.go:300-306` | Typed `SetupPending` + route-pattern exemptions vs a 409 with a hard-coded panel URL and no exemptions. |
| Point 18 route gate | `middleware/orgctx.go:387-476` | `v1/auth.go:442-491` | Two copies of the same four-branch switch; already diverge on 404-vs-403 for level refusals. |
| Defaults knob catalogue | `server/server.templ:425-468` (10 knobs), `org/defaults.templ:24-36` (4), `project/stacksettings.templ:79-87` (3, stack) and `:508-516` (3, env) | `v1/settings.go:192-255` (14 knobs) mounted at all four levels by `v1.go:367-374` | Five hand-written lists; the API publishes and accepts `build_node`, `node_group`, `metric_retention_hours`, `protect_password` and four concurrency knobs at stack and env level, where the panel offers none of them. |
| Log source fallback | `web/handler/app/handler.go:153-166` | `v1/logs.go:26-52` | API adds a `telemetry.Container` re-resolve the panel does not have. Duplication, not drift — different operations (JSON read vs SSE), and the API is the more thorough. |
| Filter a collection to the caller | — | `v1/backups.go:30` (`dests.Visible(viewer)`) vs `apps.go:34`, `stacks.go:19`, `databases.go:27`, `domainresources.go:36-43` | Divergence *inside* the API: one listing delegates to a `Viewer`-taking service, four hand-roll `orgAllowed` per row after a `ListAll`. |
| Registry tag delete | `web/handler/org/registry.go:194-224` | `v1/registry.go:166-193` | Same four-step sequence written twice; API adds path-unescape and `registry.ValidTag`, panel does not (form value, not a path segment). |
| Delete a managed instance | `web/handler/db/handler.go:420-440` (force hard-coded `false`, plus `RequireConfirm`) | `v1/databases.go:270-286` (`?force=true`) | Deliberate and documented at both ends — listed for completeness, not as drift. |
| Promote force flag | `web/handler/project/releases.go:306` (`true` after a dialogue) | `v1/releases.go:145-147` (body, default `false`) | Deliberate and documented. |
| Audit of secret reads | `middleware/audit.go:31-58` (`AuditPanelViews`, `AuditServeValue`) | `v1/variables.go:52-68,163` (`auditActor`, `auditSecretReads`) | Two actor formats (`u.Email` vs `"api:"+key.Name`) and two sets of call sites; both fired by the caller rather than by the service that reads the value. |

---

## Clean files

Nothing in these decides anything about the domain — binding, rendering,
status mapping, route/DI wiring, or plumbing only.

- `api/handler/health/handler.go` — one store health probe, two JSON shapes.
- `api/server.go` — route registration and DI **except** `nodeSamples:150-171`.
- `stream/hub.go` — in-process pub/sub, no domain vocabulary.
- `middleware/logging.go` — request id and structured logging.
- `middleware/theme.go` — copies the user's theme onto the request context.
- `middleware/actor.go` — builds `service.Actor` from the session; the
  one-function-not-three note is the right call.
- `middleware/svcerr.go` — the D4 single error mapper, explicitly excluded by
  the rubric.
- `v1/types.go` — request/response shapes and their OpenAPI tags.
- `v1/proxycfg.go` — every handler forwards to `service/proxy`.
- `v1/stacks.go` — every handler forwards to `StackService`/`EnvironmentService`
  (apart from `orgForCreate`, filed above under `helpers.go`).
- `v1/v1.go` — the route table, `op`/`opw`/`opr`, and the spec reflector. The
  verb-and-kind-at-the-route design is where authorization belongs; the file is
  listed clean on the strength of that.
- `v1/domains.go` — forwards to `DomainService`; only the repeated staged-409
  wording is noted above.
- All `*_test.go` in the shard — they assert rules, they do not own any.
