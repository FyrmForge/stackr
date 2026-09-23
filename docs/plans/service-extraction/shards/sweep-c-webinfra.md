# Sweep C — prhook + web infrastructure

Every non-generated `.go` file in `handlers/web/handler/prhook`,
`handlers/web/{graph,components,components/canvas,components/form,filebrowse,avatar,wsproxy,wsterm}`
and `handlers/web/*.go`. `*_templ.go` skipped, and
`components/staticmanifest.go` (21 lines) is `hamr gen static` output, read but
not audited.

Files read:

| File | Lines |
|---|---|
| `handler/prhook/handler.go` | 784 |
| `handler/prhook/watch.go` | 54 |
| `handler/prhook/handler_test.go` | 51 |
| `handler/prhook/watch_test.go` | 29 |
| `graph/graph.go` | 988 |
| `graph/layout.go` | 826 |
| `graph/org.go` | 208 |
| `graph/stack.go` | 200 |
| `graph/placement.go` | 156 |
| `graph/orgs.go` | 81 |
| `graph/vars.go` | 81 |
| `graph/graph_test.go` | 394 |
| `graph/layout_test.go` | 339 |
| `graph/stack_test.go` | 186 |
| `graph/org_test.go` | 171 |
| `graph/layout_stress_test.go` | 125 |
| `graph/orgs_test.go` | 93 |
| `graph/placement_test.go` | 55 |
| `graph/slicedomains_test.go` | 30 |
| `components/panel.go` | 105 |
| `components/helpers.go` | 55 |
| `components/confirm.go` | 33 |
| `components/staticmanifest.go` | 21 (generated) |
| `components/plan_test.go` | 105 |
| `components/theme_test.go` | 57 |
| `components/chart_test.go` | 29 |
| `components/initials_test.go` | 14 |
| `components/canvas/status.go` | 72 |
| `components/canvas/geom_test.go` | 81 |
| `components/canvas/status_test.go` | 75 |
| `components/form/helpers.go` | 26 |
| `filebrowse/filebrowse.go` | 177 |
| `avatar/avatar.go` | 96 |
| `avatar/handler.go` | 60 |
| `avatar/avatar_test.go` | 36 |
| `wsproxy/wsproxy.go` | 54 |
| `wsproxy/wsproxy_test.go` | 160 |
| `wsterm/wsterm.go` | 91 |
| `wsterm/wsterm_test.go` | 78 |
| `server.go` | 977 |
| `registrytoken.go` | 63 |
| `setuppending.go` | 62 |
| `htmxerr.go` | 43 |
| `e2e_test.go` | 177 |
| `gatefree_test.go` | 332 |
| `journey_test.go` | 263 |
| `journey_config_test.go` | 207 |
| `journey_branch_test.go` | 205 |
| `journey_setup_test.go` | 125 |
| `journey_access_test.go` | 61 |
| `routelevel_test.go` | 402 |
| `routeread_test.go` | 339 |
| `routeverb_test.go` | 190 |

**Already-established lines, confirmed.** `prhook/handler.go:39` does hold
`engine *deploy.Engine` (and no `DeployService`); the push-deployability rule
set is at `:424-434`; `syncPR` is at `:702-720` and re-spells the same rule at
`:719`. All three are as the index describes. Everything below is what else is
in the package.

## Findings

prhook is the worst offender in the shard: 784 lines, one handler struct
holding a store, an infra engine, an envops.Ops, a stackconf.Applier, a
githubapp client, an orgconf.Runner, a workqueue and seven services, and
essentially the whole "what does a git push do" workflow. It has no service
counterpart at all — there is no `PREnvironmentService`, no `WebhookService`,
and `DeployService` is not wired into it.

### prhook/handler.go:39 — the webhook owns the deploy engine
- **Rule:** the PR/push hook enqueues and parks deploys through `*deploy.Engine`
  directly (`:436` `engine.Park`, `:441` and `:720` `engine.Enqueue`).
- **Category:** direct `infra/` use from a handler.
- **Owner it should have:** `DeployService`. Note `DeployService.deployable`
  (`service/deploy.go:63-90`) refuses `IsManaged()` tiles and is not a drop-in:
  the webhook path deliberately deploys `cron` tiles, and `engine.Park` has no
  service equivalent at all (`DeployService` exposes `Trigger`, `Rollback`,
  `Cancel`, `RedeployIfRunning` — nothing that parks on CI).
- **Duplicated at:** the rule set itself is spelled twice inside the package,
  `:424-434` (push) and `:719` (PR synchronize).
- **Severity:** debt (bug-shaped: the two spellings can drift, and one already
  differs — the push path applies `watchMatch` and `WaitForCI`, the sync path
  applies neither).

### prhook/handler.go:424-434 — "is this push deployable"
- **Rule:** `SourceType == "git" && (Kind == "service" || Kind == "cron") &&
  GitBranch == branch && sameRepo(...)`, then `watchMatch(t.WatchPaths, ...)`,
  then the `done[t.ID]` dedupe, then `WaitForCI && ConnectorID != "" &&
  p.After != ""` → park, else enqueue.
- **Category:** branching on domain state + orchestration.
- **Owner it should have:** `DeployService` (a `DeployService.OnPush` or a
  tile-level `Deployable(t, push)` predicate).
- **Duplicated at:** `:719` (the cron/service half, without the branch, watch
  or CI parts).
- **Severity:** bug — `syncPR` ignores `WaitForCI`, so a tile gated on CI in
  its default env is deployed unparked in its PR env.

### prhook/handler.go:702-720 — syncPR
- **Rule:** a PR-env redeploy is every `git` `service`/`cron` tile in the env,
  and a missing env is silently "nothing to sync".
- **Category:** orchestration + direct engine use.
- **Owner it should have:** the same `DeployService` entry point as `:424`.
- **Duplicated at:** `:424-434`.
- **Severity:** bug (see above).

### prhook/watch.go:13 — watchMatch
- **Rule:** the whole watch-paths language: one regex per line, `!` prefix
  inverts, empty config or unknown change set always deploys, invalid regex
  lines are skipped.
- **Category:** validation/interpretation of domain shape — this is a tile
  field's semantics, living in a webhook handler package.
- **Owner it should have:** `DeployService` or the tile domain
  (`repo.Tile.WatchPaths` is the field it interprets). Repo-wide it has exactly
  one caller and one test, both inside `prhook`, so nothing else can honour it:
  the API's deploy path, the CI gate and imagewatch all deploy without it.
- **Duplicated at:** nowhere — which is the problem.
- **Severity:** debt.

### prhook/handler.go:743-762 — sameRepo / normalizeRepo
- **Rule:** a tile's git URL matches a webhook repo if the normalized
  https/ssh/bare forms agree (strip `.git`, scheme, `git@`, first `:` → `/`).
- **Category:** validation of domain shape.
- **Owner it should have:** a git/connector domain helper (`infra/githubapp` or
  `repo`), not a web handler.
- **Duplicated at:** the config-managed comparisons do the same job with a raw
  string compare instead (`stack.ConfigRepo == p.Repository.FullName` at `:140`,
  `:191`; `org.ConfigRepo == ...` at `:301`; `s.ConfigRepo != ...` at `:321`).
  That works only because the *write* side normalizes first — and it does so in
  two more hand-rolled spellings of the same `TrimPrefix("https://github.com/")
  + TrimSuffix(".git")` rule, at `service/stack.go:251` and
  `handler/org/config.go:64`. Three normalizers, none shared, and the loose
  matcher here understands ssh and bare forms that neither of the other two do.
- **Severity:** debt — bug-shaped: a config repo bound by any path that does not
  run one of the two trimmers (an import, a seed, a future connector) stops
  matching, while the tile side would still match it.

### prhook/handler.go:118 and :194 — PR config read off the raw store
- **Rule:** `repo.LoadPRConfig(ctx, h.store, stack.ID)`, while the handler is
  already holding `h.prenvs *service.PREnvService`, whose `Get`
  (`service/prenv.go:30`) is that exact call.
- **Category:** store access bypassing its owner.
- **Owner it should have:** `PREnvService.Get`.
- **Duplicated at:** `handlers/api/v1/lifecycle.go:285`,
  `handlers/web/handler/project/handler.go:2085` — four raw copies, one owner.
- **Severity:** debt.

### prhook/handler.go:139-146 vs :191-197 — which stacks a webhook is for
- **Rule:** the single-stack route (`Hook`) treats a stack as addressed when
  *either* a tile tracks the repo *or* the stack is config-managed on that repo
  (`:139-140`), and posts the plan comment for the config-managed case
  (`:144-146`). The connector route (`HookConnector`) posts the plan comment on
  the same condition (`:191-193`) but then requires `cfg.Enabled &&
  stackTracksRepo` before dispatching (`:195`) — the config-managed fallback is
  gone.
- **Category:** duplication across surfaces, already drifted.
- **Owner it should have:** one "stacks addressed by this delivery" resolver.
- **Duplicated at:** the two blocks above.
- **Severity:** bug — a config-managed stack whose tiles clone a different repo
  gets PR environments through `/hooks/github/:stack` and not through
  `/hooks/connectors/:id`.

### prhook/handler.go:260-274 — configOnlyPush
- **Rule:** "a push that touches only the stack's config file re-plans but does
  not rebuild", including the `ConfigPath == "" → stackconf.DefaultPath`
  defaulting.
- **Category:** branching on domain state.
- **Owner it should have:** `stackconf` (it already owns `DefaultPath`) or a
  stack service — the path-defaulting rule in particular.
- **Duplicated at:** the same defaulting is re-derived wherever a config path is
  read; here it is a private copy.
- **Severity:** debt.

### prhook/handler.go:284-360 — planConfigs
- **Rule:** the whole re-plan workflow. Org config plans only when
  `org.ConfigManaged() && org.ConfigRepo == repo && orgBranch == branch`, with
  `orgBranch == ""` resolved through the connector's default branch
  (`:302-307`); per-env plans only for `Type == "static" && ConfigBranch ==
  branch` (`:329`); the stack-scoped plan when `pl.StackBranch == branch`
  (`:341`); `err != stackconf.ErrNoFile` treated as fatal-but-logged.
- **Category:** orchestration (five decisions and four infra/config calls in one
  function) + side effects.
- **Owner it should have:** a config/plan service. `orgconf.Runner` and
  `stackconf.Planner` are both reached into directly from here
  (`h.orgcfg.Plan`, `pl.RunEnv`, `pl.Run`, `pl.StackBranch`).
- **Duplicated at:** the org-branch defaulting (`""` → connector default) is
  the same rule `orgconf` applies internally; the "auto policy applies, manual
  holds" rule is deferred to `Applier.holdManual` here but re-decided in the
  panel's approve paths.
- **Severity:** debt.

### prhook/handler.go:338, :355 — notifier fan-out fired by the caller
- **Rule:** after a plan lands, poke the canvas (`h.notifier.Project(s.ID)`).
- **Category:** side effect fired by a caller rather than the owner.
- **Owner it should have:** whatever writes the plan row.
- **Duplicated at:** the index already records 11 copies in
  `project/stackgraph.go` and `project/handler.go`; these two are copies 12 and
  13.
- **Severity:** debt.

### prhook/handler.go:370-383 — maybeAutoApply
- **Rule:** only a `pending` plan is auto-applied; with no work queue the
  auto-apply is silently skipped (`h.work == nil → return`, no log).
- **Category:** branching on domain state.
- **Owner it should have:** the plan/apply service.
- **Duplicated at:** the `Status != "pending"` guard is re-spelled in every
  approve handler.
- **Severity:** debt (the silent `h.work == nil` return is bug-shaped: a
  mis-wired binary deploys nothing and logs nothing).

### prhook/handler.go:389-449 — autoDeploy
- **Rule:** the push-to-deploy workflow: org filter, `configOnlyPush` skip,
  env listing, default-env filter, tile listing, per-tile deployability, park
  vs enqueue.
- **Category:** orchestration.
- **Owner it should have:** `DeployService`.
- **Duplicated at:** `stackTracksRepo` (`:479-496`) walks the same
  envs→tiles→sameRepo tree for a different answer.
- **Severity:** debt.

### prhook/handler.go:454-461 — isDefaultEnv
- **Rule:** "the default environment is the first `static` env in store order".
- **Category:** branching on domain state.
- **Owner it should have:** `EnvironmentService` — this is the env ladder's
  bottom rung and nothing else in the repo agrees on how to find it.
- **Duplicated at:** the code comment says so itself: "store order, like
  allAuto" — `stackconf`'s `allAuto` makes the same assumption separately, and
  `components/panel.go:64 CtxUpperEnv` / `envnet.UpperEnv` answers the inverse
  question with a third spelling.
- **Severity:** debt (self-documented duplication).

### prhook/handler.go:498-571 — openPR
- **Rule:** the entire "create a preview environment" workflow: no template in
  the file → do nothing (`:510`); `pre.Enabled == false` → do nothing (`:513`);
  `pre.Against` non-empty and base not in it → do nothing (`:516`); the file's
  `enabled/comment/status` overwrite the stored panel toggle (`:525-535`); env
  named `"PR 42"` from `slug[:2]` upper-cased (`:542`); `Adopt` rather than
  `Create` to get past the config gate (`:541`); a one-env `stackconf.Resolved`
  built by hand with the PR head as DefaultBranch (`:555-560`); apply; layout
  copied from `envs[0]`; scheduler reload.
- **Category:** orchestration, validation and side effects — all six categories
  in one function.
- **Owner it should have:** a `PREnvironmentService` that does not exist.
- **Duplicated at:** the "file overwrites the panel toggle" rule is the inverse
  of `PREnvService.Update` (`service/prenv.go:50-60`), which gates `comment` and
  `status` as file-owned; here the hook writes them through `Adopt`, the
  gate-bypassing door. Two rules about the same two fields, in two packages.
- **Severity:** bug.

### prhook/handler.go:566-568 — the PR env's board layout
- **Rule:** a PR env has no base env, so its card positions come from
  `envs[0]` (the oldest env), via `envops.CopyLayout(ctx, h.store, ...)` —
  a raw-store call from a handler.
- **Category:** branching on domain state + store access from a handler.
- **Owner it should have:** `GraphService` (it owns positions) or the env
  service that creates the env.
- **Duplicated at:** `config/envops/envops.go:158` does the same copy keyed on
  `env.BaseEnvID`; the handler's `envs[0]` is a second, different answer to
  "what does a new env inherit its layout from".
- **Severity:** debt.

### prhook/handler.go:569 and :737 — scheduler reload fired by the caller
- **Rule:** after creating or tearing down a PR env, `h.sched.Reload(ctx)`.
- **Category:** side effect fired by a caller.
- **Owner it should have:** the env/tile writer. `BackupScheduleService` already
  reloads from inside its own writes (`service/backupschedule.go`), so the
  pattern exists and this path does not use it.
- **Duplicated at:** every env-mutating handler in `project` and `org`.
- **Severity:** debt.

### prhook/handler.go:576-629 — updatePlanComment
- **Rule:** the plan-preview scoping decision. If the PR's base is the stack's
  own config branch, preview everything except the envs bound to *other*
  branches (`:598-604`); otherwise find the single env bound to that base and
  preview only it, and if there is none, do nothing (`:606-614`). Plus: a
  closed PR clears the stored section (`:586-589`), an invalid config renders as
  a warning rather than failing (`:622`), and the result is stashed in the
  settings table under `settings.PRPlanKey`.
- **Category:** branching on domain state + orchestration + direct
  `infra/githubapp` use (`h.gh.RefreshPRComment`).
- **Owner it should have:** the plan service. Today the write side is here and
  the read side is in `infra/githubapp/feedback.go:160`, which reads the same
  key back — two packages sharing a settings-table channel with no owner
  between them.
- **Duplicated at:** `infra/githubapp/feedback.go:89` separately does its own
  `repo.LoadPRConfig` for the same PR.
- **Severity:** debt.

### prhook/handler.go:677-691 — fileResolved
- **Rule:** "the bound config at the stack's branch, or nothing" — four
  failure modes all collapsed to `nil`, including a genuine fetch error.
- **Category:** orchestration over `stackconf.Planner` from a handler.
- **Owner it should have:** the config/plan service.
- **Duplicated at:** the same `pl.Store == nil || pl.Src == nil` wiring check
  appears at `:285` and `:679`.
- **Severity:** debt.

### prhook/handler.go:726-739 — closePR
- **Rule:** teardown through `h.ops.Teardown` (envops, i.e. config layer) plus
  the scheduler reload; "already gone" is success.
- **Category:** orchestration + direct config-layer use from a handler.
- **Owner it should have:** `EnvironmentService.Delete`.
- **Duplicated at:** the panel's env delete (`project`) takes a different route
  to the same outcome.
- **Severity:** debt.

### server.go:789-800 and :808-819 — first-boot decided twice
- **Rule:** `store.CountUsers(ctx) == 0` means "fresh install": `/login`
  redirects to `/register`, `/register` redirects to `/login` once `n > 0`.
  Two middlewares, two raw store reads, opposite senses.
- **Category:** branching on domain state, in `server.go` — a decision, not
  wiring, so in scope.
- **Owner it should have:** `AuthService`, which already makes the same call for
  the same reason (`service/auth.go:81`, "first user is an admin"). Three
  copies of "is this a fresh install" against the raw store, one of which is
  already a service.
- **Duplicated at:** `service/auth.go:81`.
- **Severity:** debt.

### server.go:204-214 — a deactivated user's session is dead
- **Rule:** the subject loader returns nil for `!u.Active`, which is what makes
  deactivation close open sessions.
- **Category:** branching on domain state.
- **Owner it should have:** `AuthService` / `RevokeService` (`RevokeService`
  exists precisely to close what a live re-check cannot reach — this is the
  live re-check, and it lives in a DI closure).
- **Duplicated at:** the login path checks `Active` separately.
- **Severity:** debt.

### server.go:289-296 — adminOnly
- **Rule:** a non-admin gets 404, not 403, on host-level pages.
- **Category:** branching on domain state (an authorization policy).
- **Owner it should have:** `AccessService`, which owns `verbLevels` and whose
  `VerbAdminRead` is asserted to conceal rather than refuse
  (`routeread_test.go:282`). The concealment rule is enforced by a closure here
  and asserted about the verb table there.
- **Duplicated at:** `middleware.Gate`'s own refusal mapping.
- **Severity:** trivial.

### server.go:871-897 — tilePage
- **Rule:** resolve `/:org/:stack/:env/:tile` by four raw-store slug lookups,
  then branch on `t.IsManaged()` to pick the db or app detail handler.
- **Category:** orchestration + store access from the route layer.
- **Owner it should have:** `AccessService`'s resolvers, which already walk
  exactly this path — `stackByPath` / `envByPath` / `tileByPath`
  (`service/tenancy.go:261-295`) — and are already wired into `deps.Access`.
- **Duplicated at:** `service/tenancy.go:261-295`, and again at
  `server.go:911-941` below.
- **Severity:** debt.

### server.go:911-941 — redirectTile
- **Rule:** the same walk in reverse (tile → env → stack → org) to rebuild the
  canonical URL, plus `middleware.RequireStackAccess` — a body gate on a route
  that is already `read(..., VerbTileRead, KindTile, "id")`.
- **Category:** orchestration + duplication.
- **Owner it should have:** the same resolver; the canonical-URL construction
  belongs next to whatever owns slugs.
- **Duplicated at:** `server.go:871-897`.
- **Severity:** debt. Note the gate is now a second authorization path on a
  gated route — `gatefree_test.go` only scans mutating handlers, so this one is
  invisible to it.

### server.go:899-907 — clientIP
- **Rule:** trust the leftmost `X-Forwarded-For` entry because traefik is the
  edge.
- **Category:** not domain logic, but the comment states it is the same rule as
  `joinClientIP` in `handler/server/joinscript.go`.
- **Owner it should have:** one place; a trust-boundary rule spelled twice is
  the kind that drifts.
- **Duplicated at:** `handler/server/joinscript.go` (shard B).
- **Severity:** trivial.

### setuppending.go:52-62 — setupOwner
- **Rule:** `IsAdmin(c) || member.Role == "owner"` in *the org the request
  addressed*, read straight off `store.GetOrgMember`.
- **Category:** branching on domain state — a role rule outside `AccessService`.
- **Owner it should have:** `AccessService`. The comment even names the hazard
  ("not necessarily the active one OrgRole answers for"), which is exactly the
  active-org vs resource-org distinction `routelevel_test.go` keeps a whole
  column for.
- **Duplicated at:** every owner check `AccessService` resolves.
- **Severity:** debt.

### components/panel.go:94-105 — EnvColor
- **Rule:** resolve an environment's colour by reading `GetStack`,
  `ListEnvironmentsByStack` and `GetOrg` off the raw store and running
  `envcolor.Map(envs, org, st.ConfigManaged())`.
- **Category:** orchestration + store access, from a *components* package.
- **Owner it should have:** `EnvironmentService` (or a small env-colour service).
- **Duplicated at:** `handler/org/settings.go:94` (`envcolor.Map(envs, o,
  false)` — a hardcoded `false` where this one passes `st.ConfigManaged()`) and
  `handler/project/handler.go:551`, `:1923`. Four call sites, each assembling
  the same three rows itself. The hardcoded `false` is currently harmless —
  `managed` only relabels `Resolved.Source` as "file" rather than "stack"
  (`envcolor/envcolor.go:110`), and that page zeroes `e.Color` first — but it is
  a third answer to the same argument.
- **Severity:** debt.

### components/panel.go:73-90 — StashUpperEnv / StashTileLocation
- **Rule:** `envnet.UpperEnv(ctx, store, t)` decides whether a tile's Deploy
  button is hidden (the env-ladder rule again), and `StashTileLocation`
  branches on `sc.EnvSlug == repo.HomeSlug` to decide a shared tile shows only
  its stack.
- **Category:** branching on domain state + direct `infra/envnet` use from a
  rendering package, both with the raw store.
- **Owner it should have:** `EnvironmentService` / `TileService`.
- **Duplicated at:** the ladder rule also lives at `prhook/handler.go:454`
  (`isDefaultEnv`) and in `stackconf`'s `allAuto`.
- **Severity:** debt.

### components/confirm.go:18-33 — Confirmed / RequireConfirm
- **Rule:** a destructive POST must carry the exact typed name; an empty
  expectation is never satisfiable.
- **Category:** validation.
- **Owner it should have:** arguably fine where it is (it reads a form value),
  but the *policy* of which operations require it is spread across callers, and
  `journey_branch_test.go:102` records a case where the wizard's Discard had to
  be exempted.
- **Duplicated at:** the API surface does not use it at all, so panel-only.
- **Severity:** trivial.

### filebrowse/filebrowse.go:39 and :126-166 — path confinement + volume backend
- **Rule:** `CleanDir` is the "must stay inside the share" rule
  (`path.Clean("/" + p)`), and `VolumeFS` is a direct `*cluster.Cluster`
  backend — `ListVolumeFiles`, `ReadVolumeFile`, `WriteVolumeFile`,
  `DeleteVolumeFile`, each carrying the "which node holds this volume" rule in
  its `Node` field.
- **Category:** validation of domain shape + direct `infra/` use.
- **Owner it should have:** a volume/storage service. The index already records
  that "No `VolumeService` exists"; this is the read/write half of the same gap,
  and the node-resolution rule here is the same one
  `handler/server/handler.go:270-320` owns for create/remove.
- **Duplicated at:** the s3 backend implements the same `Backend` interface
  elsewhere (shard A, `handler/db`), so the confinement rule is applied once but
  the *node* rule is not.
- **Severity:** debt.

### avatar/avatar.go:24-73 — upload policy
- **Rule:** 2 MiB cap, sniffed content type against a four-entry allow-list,
  stored name gets a random suffix so a replaced image never reuses a cached
  URL. `Replace` (`:78`) additionally decides a failed delete is not an error.
- **Category:** validation of domain shape + a side effect (delete-on-replace).
- **Owner it should have:** a small account/org-media service, or at minimum
  not a handler package. Two callers today (account, org) and both go through
  it, so it is not drifting — but it is the only file-upload policy in the
  codebase and it sits under `handlers/`.
- **Duplicated at:** none.
- **Severity:** trivial.

### avatar/handler.go:38-47 — clean
- **Rule:** an avatar path must be a plain file directly under `users/` or
  `orgs/`; anything else 404s. A security boundary (the storage backend may be
  a filesystem).
- **Category:** validation at a trust boundary.
- **Owner it should have:** it is tested (`avatar_test.go`) and correct; the
  finding is that `routeread_test.go:216` records `/avatars/*` as an
  authenticated-but-untenanted read — any signed-in user can fetch any avatar
  path. That is recorded as a deliberate open question, not a gap in this file.
- **Duplicated at:** none.
- **Severity:** trivial (noted so the sweep's plan does not "fix" it blindly).

### graph/graph.go:461-473 — WorstStatus
- **Rule:** the status ranking `error > building/queued > unhealthy >
  running/done`, used to collapse a group of tiles into one card light.
- **Category:** branching on domain state, and the doc comment states the
  ranking must agree with `nodeStatus` in `components/canvas/canvas.templ` and
  `statusSpan` in `frontend/static/js/graph.js`.
- **Owner it should have:** one status vocabulary, shared by the Go and JS
  renderers. Today it is three copies held in step by a comment.
- **Duplicated at:** `components/canvas/canvas.templ`,
  `frontend/static/js/graph.js`.
- **Severity:** debt (self-documented).

### graph/graph.go:669 — deploy.WaitingFor from the graph package
- **Rule:** a tile parked on an unset declared value renders as
  `waiting:<name>`; the graph package imports `infra/deploy` to decode it.
- **Category:** direct `infra/` use from a rendering package.
- **Owner it should have:** the status vocabulary above, or `repo.Tile`.
- **Duplicated at:** `config/stackconf/apply.go:1404`,
  `infra/deploy/waiting.go:48,82,103`.
- **Severity:** trivial.

### graph/graph.go:337-352 — SliceDomains / ResourceHref / resourceKind
- **Rule:** only a publicly-readable slice of an HTTP-speaking engine inherits
  its instance's hostnames; the noun and drawer path come from
  `managedtiles.Engines`.
- **Category:** branching on domain state — but correctly sourced from the
  engine registry rather than a local switch, and it has a regression test
  (`slicedomains_test.go`).
- **Owner it should have:** arguably `SliceService`; in practice this is the
  display half of a rule whose write half is elsewhere.
- **Duplicated at:** none found.
- **Severity:** trivial.

## Clean files

Pure rendering, pure transport, pure wiring, or tests. No domain decision that
belongs elsewhere.

- `graph/layout.go`, `graph/org.go`, `graph/orgs.go`, `graph/stack.go`,
  `graph/vars.go`, `graph/placement.go` — card geometry, layout engines,
  view-model structs. `placement.go` is explicitly designed to keep swarm reads
  out of `Build`, which is the right shape.
- `graph/graph.go` apart from the three findings above — `Build` is a pure
  function of the rows it is handed, which is exactly what the rubric wants.
- `components/helpers.go`, `components/form/helpers.go`,
  `components/canvas/status.go` — flash classes, static URLs, OOB field errors,
  and the status payload's markup. `status.go`'s `hasFooter` switch is display
  copy, not a domain rule.
- `wsproxy/wsproxy.go`, `wsterm/wsterm.go` — byte pumps. No domain knowledge at
  all; the `context.WithoutCancel` reasoning is transport, not policy.
- `htmxerr.go` — HTTP status mapping, explicitly out of scope per the rubric.
- `registrytoken.go` — the model for the rest of this shard: the comment says
  outright that identity and access are `RegistryService.Authenticate`'s, and
  what is left in the handler is the docker protocol.
- `server.go` outside the findings above — route wiring, DI construction, the
  `mutate`/`read` helpers, `skipOnPoll`, `RegisterStaticPages`. Out of scope by
  instruction, and the `mutate`/`read` pair is a good pattern: declaring a verb
  and mounting its check cannot be done separately.
- All test files: `prhook/{handler,watch}_test.go`, every `graph/*_test.go`,
  `components/{initials,chart,theme,plan}_test.go`,
  `components/canvas/{status,geom}_test.go`, `avatar/avatar_test.go`,
  `wsproxy/wsproxy_test.go`, `wsterm/wsterm_test.go`, and the web top-level
  suites (`e2e_test.go`, `gatefree_test.go`, `routeverb_test.go`,
  `routeread_test.go`, `routelevel_test.go`, `journey_test.go`,
  `journey_{setup,branch,config,access}_test.go`). The route tables in
  `routelevel_test.go` / `routeread_test.go` are captured data about rules, not
  a second copy of them.
