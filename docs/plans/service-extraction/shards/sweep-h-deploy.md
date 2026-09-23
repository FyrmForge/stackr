# Sweep H — deploy/jobs/workqueue/managedtiles infra

Files read: 23 non-generated `.go` files (no `*_test.go`).

| File | Lines |
|------|-------|
| `internal/stackrd/infra/managedtiles/managedtiles.go` | 633 |
| `internal/stackrd/infra/managedtiles/provision_s3.go` | 364 |
| `internal/stackrd/infra/managedtiles/engine_postgres.go` | 358 |
| `internal/stackrd/infra/managedtiles/resolve.go` | 307 |
| `internal/stackrd/infra/managedtiles/resources.go` | 238 |
| `internal/stackrd/infra/managedtiles/provision.go` | 230 |
| `internal/stackrd/infra/managedtiles/sqlbrowse.go` | 160 |
| `internal/stackrd/infra/managedtiles/s3browse.go` | 153 |
| `internal/stackrd/infra/managedtiles/scope.go` | 131 |
| `internal/stackrd/infra/managedtiles/reconcile.go` | 110 |
| `internal/stackrd/infra/managedtiles/fork.go` | 97 |
| `internal/stackrd/infra/managedtiles/infrapath.go` | 87 |
| `internal/stackrd/infra/managedtiles/ready.go` | 67 |
| `internal/stackrd/infra/managedtiles/sqlnames.go` | 44 |
| `internal/stackrd/infra/managedtiles/stats.go` | 33 |
| `internal/stackrd/infra/deploy/engine.go` | 1437 |
| `internal/stackrd/infra/deploy/waiting.go` | 123 |
| `internal/stackrd/infra/cigate/cigate.go` | 130 |
| `internal/stackrd/infra/jobs/jobs.go` | 587 |
| `internal/stackrd/infra/workqueue/workqueue.go` | 445 |
| `internal/stackrd/infra/volmove/volmove.go` | 823 |
| `internal/stackrd/infra/placement/placement.go` | 287 |
| `internal/stackrd/infra/imagewatch/registry.go` | 192 |

## Findings

### deploy/engine.go:246 — the engine's own deployability rule
- **Rule:** `if app.IsVolume() { return "volume tiles are not deployable" }` — the engine's *entire* eligibility check on the deploy entry point.
- **Category:** eligibility.
- **Owner it should have:** `DeployService.deployable` (`service/deploy.go:63`) already spells this plus four more rules (`nil`, `IsManaged`, `Kind == "cron"`, `envnet.UpperEnv`, engine-unavailable). The engine should hold none of them; every caller should enter through the service.
- **Also spelled at:** `service/deploy.go:68` (`IsVolume`, same rule, second spelling).
- **Severity:** HIGH. Not "two rule sets that disagree" — the engine is effectively *unguarded*, and four callers enter below the service (see the rule map): `prhook/handler.go:436,441,720`, `api/v1/lifecycle.go:75`, `config/stackconf/apply.go:102,105,107,154`, `volmove.go:691`. A managed instance, or an upper-env tile, reaching `apply.go:102` or `lifecycle.go:75` gets none of the four missing refusals.

### deploy/engine.go:353 — `Park` has no eligibility rule at all
- **Rule:** create a `waiting_ci` deployment row for any tile handed to it. Zero checks — not even the `IsVolume` one `Enqueue` has.
- **Category:** eligibility.
- **Owner it should have:** a `DeployService.Park` that runs the push-path rule set (see the cron note below) before writing the row. No service equivalent exists today.
- **Also spelled at:** ABSENT from `service/`. Its only caller is `prhook/handler.go:436`, a handler that holds `*deploy.Engine` and no `DeployService`.
- **Severity:** HIGH — a request-reachable write path with no owner.

### deploy/engine.go:246 vs service/deploy.go:73 — the cron rule is two different questions, not drift
- **Rule:** `DeployService.deployable` refuses `t.Kind == "cron"` ("cron services run on their schedule; use Run now"); the webhook path at `prhook/handler.go:424` *deliberately* deploys cron tiles (`t.Kind == "service" || t.Kind == "cron"`), and `engine.pipeline:685` has a first-class non-keep-alive branch that stops a cron at the build.
- **Category:** eligibility — but two rules wearing one name.
- **Owner it should have:** split them. "A person clicking Deploy on a cron gets told to use Run now" is a *surface* rule (panel/API affordance). "A push rebuilds a cron's image and stops at the build" is a *domain* rule and belongs in the same service, as a separate entry point (`DeployService.Build`/`TriggerPush` vs `Trigger`).
- **Also spelled at:** `service/deploy.go:73` (refuse), `prhook/handler.go:424` (allow), `deploy/engine.go:685` + `deploy/engine.go:548` (build-only handling), `jobs.go:354` (`a.Kind != "cron"` schedule filter).
- **Severity:** HIGH. Do **not** resolve this by moving `deployable` into the engine — that would break the push path on purpose-built behaviour.

### deploy/engine.go — the upper-environment rule, third spelling
- **Rule:** "only the bottom rung of the ladder takes a push/deploy; higher envs take promotes."
- **Category:** eligibility / precedence.
- **Owner it should have:** `DeployService`, one predicate.
- **Also spelled at:** `service/deploy.go:76` (`envnet.UpperEnv`), `prhook/handler.go:413` (`env.Type != "static" || !isDefaultEnv(envs, &env)` — a hand-rolled second reading, with `isDefaultEnv:454` admitting it relies on store order), `deploy/engine.go` ABSENT (the engine will happily deploy into any env).
- **Severity:** HIGH — two spellings that can disagree, and the enforcing layer is a handler.

### deploy/engine.go:251,312,354,392 — supersede precedence spelled twice
- **Rule:** "a newer deploy of the same tile outranks an older one still waiting." `SupersedeWaiting` walks `Waiting()` and cancels by hand; the work queue does the same thing by dedupe key (`workqueue.go:182`, key = tile id).
- **Category:** precedence / dedupe policy.
- **Owner it should have:** `DeployService`. The engine's own comment at `engine.go:103` concedes the two are the same rule ("the dedupe key is the tile, which is exactly what SupersedeWaiting does by hand").
- **Also spelled at:** `workqueue.go:182-234` + the `OnSuperseded` hook at `engine.go:130`; ABSENT from `service/`.
- **Severity:** MEDIUM — coherent today, but one rule with two implementations and two failure modes.

### deploy/engine.go:129-146, :155-169 — restart/recovery policy
- **Rule:** a deploy is convergent, so requeue it on restart (`OnRestart: Requeue`); `reopen` decides which statuses count as "interrupted" (`running`, or `error` with `repo.InterruptedMsg`) and rewrites the row back to `queued`; a superseded queued row becomes `cancelled` with a fixed message.
- **Category:** retry / scheduling policy + domain-state branching.
- **Owner it should have:** `DeployService` (it already owns `Record`/`Progress`/`Row`). The engine should perform the run, not decide what a restart means for a deployment.
- **Also spelled at:** the boot sweep `cmd/stackrd/main.go:365 store.SweepStaleRuns` writes the same statuses from the other side.
- **Severity:** MEDIUM.

### deploy/engine.go:528-553 — deploy outcome → tile status mapping
- **Rule:** cancelled ⇒ tile `stopped`; unset varref ⇒ `waiting:<name>`; other error ⇒ `error`; success ⇒ `running`, or `idle` when `runpolicy.For(app.Kind)` says not keep-alive.
- **Category:** branching on domain state (four-way policy over the tile's lifecycle column).
- **Owner it should have:** the tile-status owner (`service` side of `SetTileStatus`), fed a terminal deploy outcome.
- **Also spelled at:** `volmove.go:578,684,689` writes `running`/`error` for managed instances by hand; `jobs.go:444-452` maps a run's outcome to `ok`/`error`/`stopped` with its own table.
- **Severity:** MEDIUM.

### deploy/engine.go:559-571 — notifier fan-out and OnFinish from infra
- **Rule:** on every terminal deploy, publish project + container notifications and push a `KindDeployFailed`/`KindDeployDone` notification; then call `OnFinish`, whose one implementation (`cmd/stackrd/main.go:493`) branches on `tile.Kind == "function" && tile.RunOnDeploy` and writes the image digest.
- **Category:** side effects fired by a mechanism layer; domain branching in a boot callback.
- **Owner it should have:** a service that owns "a deploy finished" and decides who is told.
- **Also spelled at:** `cigate.go:128`, `jobs.go:463`, `managedtiles.go:554` (digest baseline, same write from a third place).
- **Severity:** MEDIUM.

### deploy/engine.go:763 — replica rule for pinned tiles
- **Rule:** `place.Pinned && app.Replicas > 1` ⇒ refuse ("one mounter per volume, always").
- **Category:** validation of domain shape, at deploy time.
- **Owner it should have:** tile validation (`service/tilevalidate.go`, which already imports `placement.IsPinned` at :289) — so the config file and the panel refuse it at save time, not a build that already fetched and pushed an image.
- **Also spelled at:** `placement.For:139` (`p.Replicas = 1`, silently) — the same rule with a different outcome: clamp here, refuse there.
- **Severity:** MEDIUM — silent clamp vs hard refusal for one condition.

### deploy/engine.go:586-618 — which image a deploy runs (precedence chain)
- **Rule:** rollback ⇒ `d.ImageTag`; a preset tag that exists locally ⇒ run it; `SourceType == "image"` ⇒ pull `app.ImageRef`; otherwise git build. Plus the registry-auth choice (`ghcr.io` prefix ⇒ connector creds, else managed creds).
- **Category:** precedence / defaulting.
- **Owner it should have:** arguably stays (it is the pipeline), but the *decision table* is domain policy the CLI's `plan` and the releases page re-derive elsewhere.
- **Also spelled at:** `jobs.go:522 appImage` picks a different precedence (current image, else `app.ImageRef`) for the same tile.
- **Severity:** LOW-MEDIUM.

### deploy/engine.go:1028,1038-1072 — builder identity and the host-quarter memory rule
- **Rule:** `BuilderFor(orgSlug) = "stkr-"+orgSlug`; with a build node set, one shared buildkit service, else a per-org builder sized at `info.MemTotal/4`.
- **Category:** naming/identity + a defaulting rule with no setting behind it (the comment says "a setting when someone needs to tune it").
- **Owner it should have:** naming can stay; the quarter-of-host default is a settings decision.
- **Severity:** LOW.

### deploy/engine.go:864-883 — image reference naming
- **Rule:** `<repo>:<sha7>`, `+ "-" + d.ID[:8]` on a rebuild of the same commit; `shortSHA` falls back to the deploy id.
- **Category:** naming and identity.
- **Owner it should have:** fine as mechanism, but it is the identity every rollback/promote target is addressed by, and `EnqueuePromote:284` re-derives the same string independently (`sc.ImageRepo(app.Slug) + ":" + commitSHA[:7]`).
- **Also spelled at:** `engine.go:284` vs `engine.go:869` — two constructions of one name.
- **Severity:** LOW-MEDIUM (drift risk: :284 has no rebuild suffix, so a promote can address a tag that :869 renamed).

### deploy/engine.go:819-851 — image retention policy
- **Rule:** keep the images of the last 5 successful deployments, across every env's sibling tile with the same slug; delete the rest by label.
- **Category:** retention policy (a domain decision about what a rollback can still reach).
- **Owner it should have:** the deployments owner; `keepImages = 5` is a policy constant sitting next to a docker call.
- **Severity:** LOW.

### deploy/waiting.go:35 `DepWaiting` vs jobs.go:167 `depBlocked` — `depends_on` read two ways
- **Rule:** deploy reads `depends_on` as "is a dependency parked on an unset variable" and parks this tile on the same name; jobs reads the same field as "is the dependency running / has it completed" and skips the tick.
- **Category:** branching on domain state; duplication with divergent semantics.
- **Owner it should have:** one dependency predicate in `service/`, with the two questions as two methods over one parse. Both files even carry the same comment about not importing `stackconf.ParseDep` because of an import cycle — the cycle is the symptom of the rule being below the line.
- **Also spelled at:** `deploy/waiting.go:36-55`, `jobs.go:167-184`, `jobs.go:195-207`.
- **Severity:** HIGH — one config field, two parsers, two meanings, neither owned.

### deploy/waiting.go:15,72,94,113 — the `waiting:` status protocol and its release rules
- **Rule:** a tile's status column doubles as "parked on variable `<name>`"; `ClearWaiting`/`ClearWaitingEnv`/`ClearWaitingOrg` decide the *scope* at which setting a variable releases parked tiles (stack / env / org), and that a released tile goes to `stopped` rather than being redeployed ("setting a variable is not a request to ship").
- **Category:** branching on domain state + scope/precedence policy.
- **Owner it should have:** the variable service together with the tile-status owner. Three near-identical functions differing only in which set of tiles they walk is the tell.
- **Severity:** MEDIUM.

### jobs.go:122-136, :188-209, :404 — overlap and skip policy
- **Rule:** a ref already running is skipped unless `app.AllowOverlap`; a schedule tick is skipped while the stack is held mid-apply, or while a `depends_on` target is unmet; manual runs never skip.
- **Category:** scheduling policy.
- **Owner it should have:** a cron/run service. `Hold`/`Release` (:141,:147) are a lock API that `config/stackconf` drives from outside.
- **Severity:** MEDIUM.

### jobs.go:354 — which tiles get a schedule
- **Rule:** `a.Kind != "cron" || a.Cron == "" || a.Status == "paused"` ⇒ no cron entry. "Paused" as a *status* string doubling as a scheduling switch.
- **Category:** eligibility + status-as-flag.
- **Owner it should have:** the tile service (it owns the status column and already knows what `paused` means).
- **Severity:** MEDIUM.

### jobs.go:427-429 — cron command semantics differ from service command semantics
- **Rule:** a cron's `command:` runs as `sh -c <command>`, replacing the entrypoint; a service's `command:` is shell-tokenized argv appended to the entrypoint (`engine.go:1221 serviceCommand`, `:1239 splitCommand`).
- **Category:** domain semantics of one config field, spelled per lifecycle.
- **Owner it should have:** documented and owned in one place; the engine's comment at :1220 already has to explain the other half ("Cron keeps its `sh -c` semantics in jobs.RunApp; the two lifecycles differ on purpose").
- **Severity:** MEDIUM — deliberate, but discoverable only by reading two packages.

### jobs.go:463 — cron failure notification transition rule
- **Rule:** notify only on the `ok → error` transition, read off `app.LastStatus`.
- **Category:** side effect + branching on domain state.
- **Owner it should have:** the notification/run owner.
- **Severity:** LOW.

### jobs.go:522 `appImage` — the duplicate is gone, the fallback is the finding
- **Rule:** now delegates to `Deploys.CurrentImage` (the known `appImage` loop duplication of `DeployService.CurrentImage` has been removed — `service/deploy.go:203-226` documents it). What remains here is the *fallback policy*: "no successful deployment ⇒ run `app.ImageRef`".
- **Category:** defaulting rule.
- **Owner it should have:** defensible where it is (the comment argues the fallback is a job's own), but it is the one place a tile can run an image no deployment ever produced.
- **Severity:** LOW — resolved duplication, note only.

### jobs.go:281 — "can this run be stopped" decided off the raw store
- **Rule:** `s.store.LatestWorkItem(ctx, Kind, runID)`, then `w.Done()`, then "if the item was merely queued, close the run row" — an eligibility decision made by reading `work_items` directly.
- **Category:** eligibility read with no owner.
- **Owner it should have:** `WorkItemService` (`service/workitem.go`), which today has **only write methods** (`Enqueue`, `Claim`, `Finish`, `Requeue`, `Progress`, `Supersede`) and no reads at all.
- **Also spelled at:** `volmove.go:192,346,355`, `workqueue.go:216,250,274,337`, `infra/backup/work.go:145` — see next finding.
- **Severity:** HIGH.

### workqueue.go:216,250,274,337 (+ volmove.go:192,346,355, jobs.go:281, backup/work.go:145) — the work queue has a half owner
- **Rule:** every *write* to `work_items` goes through the `Items` interface (`workqueue.go:130`, satisfied by `service.WorkItemService`), but every *read* goes straight to `repo.Store`. `ListWorkItemsByStatus` drives recovery and the drain loop; `GetWorkItem` backs `Cancel`; `LatestWorkItem` backs three eligibility decisions in other packages.
- **Category:** no service owner for the table's read half; two of the reads are eligibility, not UI.
- **Owner it should have:** `WorkItemService` gains `ListByStatus`/`Get`/`Latest`, and the eligibility questions become named methods ("is a move in flight for this tile", "is this run still stoppable").
- **Also spelled at:** the only service-layer reader is `service/plan.go:208`; everything else is infra reaching past the owner.
- **Severity:** HIGH — the guard tests could never have seen this, and `service.WorkItemService`'s own doc admits it is a forwarder.

### workqueue.go:89-114, :164-174, :354-361 — per-kind scheduling policy
- **Rule:** default timeout 30 min, default concurrency 1, `Limit` read from server settings on every drain, a kind at its limit is skipped rather than waited on, restart recovery is `Requeue` or `Fail` per kind with a per-kind message.
- **Category:** retry / scheduling policy.
- **Owner it should have:** defensible as the queue's own mechanism — but the *choice per kind* (`deploy` requeues, `cron` fails, `volmove` fails) is domain policy currently declared at each infra package's `WithWork` (`engine.go:125`, `jobs.go:248`, `volmove.go:174`).
- **Severity:** LOW-MEDIUM.

### workqueue.go:287,349 — a kind with no handler is failed
- **Rule:** an item whose kind is no longer registered is closed as `error`. A deployment/plan row it carried is *not* closed with it (only `OnSuperseded` does that, and only for supersession).
- **Category:** recovery policy with a domain consequence.
- **Severity:** LOW.

### volmove.go:314-350 — move eligibility, five rules, no service
- **Rule:** no home node ⇒ refuse; empty target ⇒ refuse (with an explicit note that `IsSelf("")` answers true, so the empty string would silently mean the manager); same node ⇒ refuse; both agents must be reachable; one move per tile, decided by reading `LatestWorkItem` and `w.Done()`.
- **Category:** eligibility.
- **Owner it should have:** a volume/move service. Today `volmove.Service` *is* the top of this stack and is called straight from handlers.
- **Severity:** HIGH — a full eligibility rule set living in infra with no owner above it.

### volmove.go:243-268 — "which volumes does this tile hold" is three rules
- **Rule:** volume tile ⇒ its own docker volume; managed instance ⇒ `managedtiles.VolumeName(t)`; otherwise ⇒ every attached volume subtile, and none ⇒ error.
- **Category:** branching on domain state; a domain question ("what data does this tile own") answered in the mover.
- **Also spelled at:** `placement.IsPinned:78-92` answers the neighbouring question with a fourth reading (`t.Volumes != ""`, `t.Engine != ""`, local storage pools, attached siblings); `placement.NodeOf:48-76` a fifth.
- **Severity:** HIGH — one domain concept, three packages, and the doc comments record two past data-loss incidents caused by the readings disagreeing.

### volmove.go:672-701 — "how is this tile started" is a domain switch
- **Rule:** `t.IsManaged()` ⇒ `managedtiles.Deploy` + write status `running`/`error` by hand; otherwise ⇒ `Deploy.EnqueueCurrent` and poll the deployment row; `id == ""` ⇒ nothing to wait for.
- **Category:** orchestration + branching on domain state, plus a status side effect the comment says "belongs to the caller".
- **Owner it should have:** one "start this tile where it is" service method; every caller of it (move, config apply, panel Deploy) currently re-derives the managed/non-managed split.
- **Severity:** HIGH.

### volmove.go:399-449, :574-592, :655-664 — rollback and cleanup policy
- **Rule:** which phase rolls back how; a start that timed out but came up on the target is *not* a failure (`cameUp`, `startGrace = 5m`); source volumes are never deleted.
- **Category:** recovery policy — correct, well-argued, and entirely undiscoverable from `service/`.
- **Severity:** MEDIUM (below the line, but single-spelled and coherent).

### volmove.go:713-744 — deployment terminal-state set, hand-written
- **Rule:** `done` ⇒ ok; `error`/`cancelled`/`stopped` ⇒ fail; anything else ⇒ keep polling.
- **Category:** duplication of a predicate that already exists.
- **Also spelled at:** `internal/deploystate` (`IsLive`, `IsCancellable`) — used by `service/deploy.go:134` and `engine.go:340`, ignored here. `service/deploy.go:126` explicitly records that four hand-written copies of this set had already drifted once.
- **Severity:** MEDIUM — a fifth copy.

### placement.go:129-213 `For` — a resolver that writes
- **Rule:** resolves group + replicas + pinning, and **persists** a home node (`SetHomeNode` at :177 and :207) the first time a pinned tile deploys.
- **Category:** placement policy + a write from what reads as a query.
- **Owner it should have:** the rules are correct and single-spelled; their address is wrong. A `PlacementService` owning the home-node column, with the chooser below it as a pure function.
- **Also spelled at:** the tell is `InGroup:100-113`, a *predicate* that has to pass `nil` for `Tiles` and guard on `t.HomeNode == ""` to defend itself against its own package's writer ("a predicate must not write to the row it is asked about").
- **Severity:** MEDIUM — reachable from handlers (`server/nodes.go:354`) and config apply (`apply.go:1971`).

### placement.go:78-92 `IsPinned` — "does this tile hold data" in five clauses
- **Rule:** volume tile, non-empty `Volumes`, non-empty `Engine`, any local storage pool, or any attached volume sibling ⇒ pinned; **an error reading siblings also means pinned**.
- **Category:** branching on domain state — the single most consequential predicate in the shard (its comment: "a stateless tile pinned by mistake still runs, a stateful one spread across nodes loses data").
- **Owner it should have:** the tile service. Six callers span service, handlers, config and three infra packages.
- **Severity:** MEDIUM (correct and single-spelled, but a domain classification living in a placement helper).

### placement.go:219-246 `ParseAttachment` — storage line grammar
- **Rule:** `storage-slug/path-name:/mount[:ro]`; mount must be absolute; a bare pool slug must name a path unless the source is a `${{` reference.
- **Category:** validation of domain shape.
- **Owner it should have:** the storage/config owner. The comment concedes the `${{` special case exists only because importing `varref` here would cycle — the same cycle tell as `DepWaiting`.
- **Also spelled at:** no external callers, but `storagetiles.Resolve` (`engine.go:734`) parses the same lines for the deploy path, and `engine.go:733`/`placement.go:267` each re-implement "blank and `#` lines are not attachments".
- **Severity:** MEDIUM.

### imagewatch/registry.go:31 `ParseRef` — image identity defaults
- **Rule:** no host segment ⇒ `registry-1.docker.io`; `docker.io`/`index.docker.io` normalised; no tag ⇒ `latest`; single-segment Hub repo ⇒ `library/` prefix; a digest-pinned ref (`@`) ⇒ **not watchable**.
- **Category:** naming and identity + one eligibility rule (`ok=false` means "never check this tile").
- **Owner it should have:** the identity rules are legitimately mechanism; the "digest-pinned is unwatchable" clause is the eligibility half and is consumed at `service/imagewatch.go:122 imageWatchable`.
- **Severity:** LOW.

### imagewatch — no policy finding (stated deliberately)
The "when is an image considered updated, and what happens then" decision is **not** in this package. It lives in `service/imagewatch.go`: the policy gate at :113 (`UpdatePolicy != "notify" && != "auto"`), the digest comparison at :133-148 (`LatestDigest` then `ImageDigest`), the `switch t.UpdatePolicy` at :151, and `autoUpdate` at :170 (managed ⇒ `Instances.Deploy`, cron/function ⇒ `PullImage`, else ⇒ `Deploys.Trigger`). `registry.go` is HTTP mechanism plus `ParseRef`. **This is the correctly-layered example in the shard.** One note: `autoUpdate` reaching `Deploys.Trigger` means auto-update *does* go through `DeployService`, so an auto-updating cron tile is refused by `deployable:73` while a pushed cron is not — the cron split again.

### managedtiles/resolve.go:60-78, :164-168 — an authorization boundary implemented in infra
- **Rule:** `ResolveScope.AllowedOrgs` bounds resolution to the caller's orgs; a path outside them returns *the same error* as one that does not exist ("the difference is exactly what must not leak"); `SystemScope()` (:74) is the deliberate, greppable bypass.
- **Category:** access control — a domain rule of the highest consequence, sitting in an infra addressing helper.
- **Owner it should have:** a service that takes the caller's identity, rather than a struct each handler must remember to fill.
- **Checked:** the two request-driven callers do pass it — `api/v1/resolve.go:28` (plus `requireEnvAccess` before enabling relative paths) and `api/v1/databases.go:199`. So today this is **misplaced, not bypassable**. The risk is structural: a nil `AllowedOrgs` silently means "everything", so the failure mode of a forgotten field is a tenancy leak, not an error.
- **Severity:** HIGH (by consequence), not currently exploited.

### managedtiles/infrapath.go:28 `ResolveInfraPath` — the unbounded sibling of `ResolveTarget`
- **Rule:** resolves `org:[stack:[env:]]slug` to an instance tile with **no scope check whatsoever**.
- **Category:** the same access-control rule as above, absent.
- **Owner it should have:** it should not exist beside `ResolveTarget`; if it must, it should take a `ResolveScope`.
- **Also spelled at:** `resolve.go:159 resolveAbsolute` does the same walk *with* the check. Caller today is `config/stackconf/slices.go:29` (a server-side applier, so currently safe).
- **Severity:** **debt** — downgraded from HIGH (`12-business-logic-plan.md` §2 #3). One caller, system-scope by design, and this entry's own text concedes it is currently safe. It should take a `ResolveScope` or not exist beside `ResolveTarget`, but it is not a live hole. Do not re-inflate.

### managedtiles/resolve.go:86-115 — relative-vs-absolute precedence and ambiguity
- **Rule:** try relative first, then absolute; if both resolve to different things the path is ambiguous and errors rather than picking; narrowest scope wins (env → stack → org).
- **Category:** precedence / naming rules.
- **Owner it should have:** an addressing service. The comment at :136 notes `stackconf.resolveFrom` "already uses" the same order — a second implementation of the precedence.
- **Severity:** MEDIUM.

### managedtiles/scope.go:81-106 `Eligible` / `ServesEnv` — sharing-scope rule
- **Rule:** `env` ⇒ same environment, `stack` ⇒ same stack, `org` ⇒ same org; plus "must be managed and its engine must support provisioning".
- **Category:** eligibility.
- **Owner it should have:** `service/slice.go` already calls it (:74, :219) — the rule should live there, with infra doing the lookup.
- **Severity:** MEDIUM — well-factored (`ServesEnv` deliberately routes through `Eligible`), just below the line.

### managedtiles/provision.go:35 + provision_s3.go:31 + provision_s3.go:216 — the public-slice gate, three spellings
- **Rule:** (a) `public && !eng.PublicSlices` ⇒ refuse (`provision.go:35`); (b) a public bucket needs the instance to carry a public domain (`provision_s3.go:31`); (c) **both again**, re-spelled inside `SetBucketPublic` (`provision_s3.go:212-218`).
- **Category:** eligibility, duplicated.
- **Owner it should have:** one `CanBePublic(instance) error` on the slice service.
- **Severity:** HIGH — three spellings of one refusal, and the create path and the flip path each carry their own copy.

### managedtiles/provision.go:49-86 `ProvisionSlice` — the adoption rule
- **Rule:** an existing slice with the same name on the same instance is *adopted* (re-stamped with the config slug, revived from `orphaned`) rather than uniquified into a second copy; the same name in a **different env** is a hard error; a `public` mismatch flips the bucket policy as a side effect.
- **Category:** orchestration + precedence ("the file names the slice it means").
- **Owner it should have:** the slice service — this is config-apply semantics, not a docker/psql mechanism.
- **Severity:** HIGH.

### managedtiles/provision.go:107-135 `AttachExisting` — same-env rule
- **Rule:** "can only attach to databases in the same environment", because the url secret is env-scoped.
- **Category:** eligibility.
- **Also spelled at:** `scope.go:102 ServesEnv` exists precisely because this rule is enforced *here*, at attach, and had to be anticipated at provision time — one rule, checked in two places from two directions.
- **Severity:** MEDIUM.

### managedtiles/provision.go:152-181 `DropDB` — teardown fan-out
- **Rule:** find every provision row naming the slice, drop the backing object *once*, then drop each row's resource mirror and the row; an engine with no `Drop` hook refuses.
- **Category:** orchestration with a decision between steps.
- **Severity:** MEDIUM.

### managedtiles/resources.go:30-44, :59-114 — resource identity and rename precedence
- **Rule:** `ResourceSlug` = the config key if present, else `instance.Slug + "-" + p.DBName`; resource identity is `(provider, slice name)` **not** the slug, so a key rename moves the row; a detached row holding the wanted slug is adopted or removed.
- **Category:** naming and identity + precedence. This is the rule that decides what `${{ tile.<slug>.X }}` resolves to.
- **Owner it should have:** the resource/reference owner in `service/` (`service/slice.go:143` already composes `Ref` + `DefaultOutput`).
- **Severity:** HIGH — a reference-resolution identity rule living three layers below the resolver that depends on it.

### managedtiles/resources.go:118-136, :155-183 — what a slice publishes, and reference GC
- **Rule:** admin credentials are never published as outputs; `AutoInjectVars` derives a consumer's wiring from the outputs themselves; on detach/drop, the consumer's `tile.<slug>.` references are **deleted from its env blob and its variable rows** because an unbound reference is a hard resolve error.
- **Category:** security policy + a destructive side effect on another tile's config, fired from infra.
- **Owner it should have:** the variable service (it owns `UpsertVariable`/`RemoveVariable`, which this reaches through `Rows`) together with the slice service.
- **Severity:** HIGH — infra editing a consumer tile's `Env` text (`resources.go:173 t.Env = strings.Join(kept, "\n")`).

### managedtiles/managedtiles.go:126-217 — the engine registry is a policy table
- **Rule:** per engine: default image, port, data path, whether it speaks HTTP (and so may take a domain), primary output, whether slices can be public, whether the whole output set must be auto-injected, slice naming function, UI nouns, picker order.
- **Category:** defaulting + eligibility, as data.
- **Owner it should have:** legitimately a registry; the finding is *who reads it*. `Engines[...]` is indexed directly from handlers and templ files in ~20 places (`web/handler/db/panel_templ.go`, `web/graph/graph.go:321,338,348`, `api/v1/forward.go:193`, `config/stackconf/plan.go:663`, …) with no service wrapper anywhere.
- **Severity:** MEDIUM — the map itself is fine; the absence of an owner between it and the UI is the finding.

### managedtiles/managedtiles.go:349-367 `NewDB` — credential and identity defaulting
- **Rule:** empty `ImageRef` ⇒ engine default (a set one is a deliberate per-instance pin); `DBName = DBUser = d.Slug` ("slug, not display name: db identifiers must not contain spaces"); `RootUser` override; a fresh 16-byte hex password.
- **Category:** defaulting + naming.
- **Owner it should have:** called from `service/managedinstance.go:170`, but also from `config/stackconf/apply.go:1236`, `orgconf/orgconf.go:1011` and `envops/envops.go:110` — four call sites, one of them a service.
- **Severity:** MEDIUM.

### managedtiles/managedtiles.go:427-445 `PublishConnection` — a write-and-audit rule in a package function
- **Rule:** upsert each connection var on the instance tile; **record an audit row only when a secret value actually changed** ("a row per deploy per credential would bury the writes that mean something").
- **Category:** side effect + auditing policy.
- **Owner it should have:** the variable service; the doc comment on `Rows` (`managedtiles.go:281`) already states the motivation — "writing them here directly is what let a managed tile publish a connection secret with none of the variable service's auditing".
- **Also spelled at:** called from `apply.go:1267,:1489`, `orgconf.go:1017`, `envops.go:133`, `managedinstance.go:176`, `managedtiles.go:491` — six call sites for one invariant.
- **Severity:** HIGH.

### managedtiles/managedtiles.go:476-558 `Deploy` — a second deploy path with its own rules
- **Rule:** managed instances are always pinned, always one replica, always get a 60s stop grace, claim a shared net only when provisions exist, and baseline their image digest themselves because "managed deploys bypass the engine's OnFinish".
- **Category:** orchestration — an entire parallel deploy pipeline.
- **Owner it should have:** the fact that `DeployService.deployable:70` refuses managed tiles ("a managed database is not deployed; it is provisioned") while this method deploys them is the shape of the problem: the service refuses the word and a different package does the thing.
- **Severity:** HIGH.

### managedtiles/managedtiles.go:599-617 `Start` / `Stop` — lifecycle fallbacks
- **Rule:** no service name, or `ServiceStatus` errors ⇒ full `Deploy`; otherwise scale to 1 and wait 30s ("shorter than a deploy's: nothing is being pulled").
- **Category:** branching on domain state.
- **Severity:** MEDIUM.

### managedtiles/reconcile.go:21-50, :65-83 — desired-state reconciliation policy
- **Rule:** every provision row is desired state; a consumer's deploy recreates missing slices but **never fails on a down instance** (best-effort by design); the instance-side self-heal recreates every slice when the instance returns; readiness is cached per instance per pass (30s consumer-side, 60s instance-side).
- **Category:** reconciliation policy + timeouts as policy constants.
- **Owner it should have:** the slice service. Fired from `engine.go:701` (the deploy pipeline constructs a `managedtiles.Service` inline to call it).
- **Severity:** MEDIUM.

### managedtiles/fork.go:34-67 `ForkSlice` — fork naming and safety rules
- **Rule:** default name `<src>-fork`, default slug `<ResourceSlug>-fork` uniquified per env; **a fork is never public** ("inheriting a source bucket's world-readability would publish that experiment"); a failed copy drops the empty destination ("an empty fork is worse than no fork").
- **Category:** naming + a security default + a compensating action.
- **Owner it should have:** the slice service.
- **Severity:** MEDIUM.

### managedtiles/sqlnames.go:15-44 — SQL identity rules
- **Rule:** `sqlIdent` folds a slug to `[a-z0-9_]`, never leading with a digit (`db_` prefix); `uniqueSliceName` suffixes with the engine's own separator until free on that instance.
- **Category:** naming and identity. Reached from config diffing via `SliceName` (`managedtiles.go:119`, called at `stackconf/plan.go:818`) — so this function decides whether a config plan shows a change.
- **Severity:** LOW-MEDIUM.

### managedtiles/engine_postgres.go:227-267 — credential-safety and idempotence rules
- **Rule:** `SET log_min_error_statement = PANIC` as the first statement of every provisioning session, because postgres logs a failing statement verbatim and ours carry plaintext passwords; `pgEnsure` checks state first and only creates what is missing, `ALTER ROLE` always re-syncs the password.
- **Category:** security measure + reconciliation. Correct and engine-local.
- **Severity:** LOW (noted, not misplaced).

### managedtiles/engine_postgres.go:24-72, provision_s3.go:28-75 — per-consumer isolation model
- **Rule:** postgres cuts a dedicated role + database and `REVOKE CONNECT ... FROM PUBLIC`; s3 gives every consumer the instance's **shared root credentials** and treats one bucket as the isolation boundary.
- **Category:** security policy, differing per engine.
- **Owner it should have:** engine-local is right; it is recorded here because "what isolates one consumer from another" is a domain guarantee readable only from these two functions.
- **Severity:** LOW (documented, deliberate).

### managedtiles/sqlbrowse.go:31-160, s3browse.go — content access scoping
- **Rule:** "scoping is enforced by credentials" — the browser connects as whatever `PGCreds` the caller resolved, and postgres grants do the rest. The s3 browser, by contrast, always uses **root** credentials (`s3ClientFor:20`), so bucket scoping depends entirely on the handler passing the right bucket name.
- **Category:** access control delegated to the caller.
- **Owner it should have:** the callers are `web/handler/db/data.go:86,:91` (PGCreds) and the file browser — a service should resolve the scope, not each handler.
- **Severity:** MEDIUM — asymmetric between the two engines, and the s3 side has no credential boundary at all.

## Engine-vs-DeployService rule map

| Rule | infra/deploy | service/deploy.go | Agree? |
|------|--------------|-------------------|--------|
| nil tile ⇒ not found | ABSENT | :65 | disagree (engine would nil-panic / proceed) |
| volume tile not deployable | `engine.go:246` (Enqueue only) | :68 | agree, spelled twice |
| managed instance not deployable | ABSENT | :70 | **disagree** — `managedtiles.Deploy` deploys them anyway |
| cron not deployable | ABSENT | :73 | **disagree on purpose** — `prhook:424` deploys crons; `engine.go:685` has the build-only branch |
| upper env refuses direct deploy | ABSENT | :76 (`envnet.UpperEnv`) | **disagree** — third spelling at `prhook:413`, engine has none |
| engine unavailable ⇒ ErrUnavailable | n/a | :84 | n/a |
| rollback needs an image tag | ABSENT (`EnqueueRollback:269`) | :55 | **disagree** — `api/v1/lifecycle.go:75` calls the engine directly, skipping :55 and all of `deployable` |
| promote needs ≥7-char commit | `engine.go:277` | ABSENT | engine-only; `apply.go:107,154` is the only caller |
| supersede older waiting deploys | `engine.go:251,312,354,392` | ABSENT | engine-only, duplicated by the queue's dedupe key |
| park a push behind CI | `engine.go:353` (no checks) | ABSENT | engine-only, request-reachable from `prhook:436` |
| release / fail a parked deploy | `engine.go:371,381` | ABSENT | engine-only; `cigate.go:103,112,123` drives it |
| redeploy-if-running predicate | ABSENT | :114 (`Kind=="service" && !managed && !volume && status=="running"`) | service-only |
| "is this deployment still live" | `engine.go:340` (`deploystate.IsCancellable`) | :134 (`deploystate.IsLive`) | agree via `internal/deploystate` — except `volmove.go:737` hand-writes a fifth copy |
| current image of a tile | `engine.go:291` → `rows.CurrentImage` | :215 | agree (the `jobs.appImage` duplicate was removed) |
| tile status after a deploy | `engine.go:528-553` | ABSENT | engine-only |
| replicas vs pinning | `engine.go:763` refuse / `placement.go:139` clamp | ABSENT (`tilevalidate.go:289` uses `IsPinned` for something else) | **disagree** — refuse vs silent clamp |

Callers entering below `DeployService` (bypassing every service rule):
`prhook/handler.go:436,441,720` · `api/v1/lifecycle.go:75` · `config/stackconf/apply.go:102,105,107,154` · `volmove.go:691` · `cigate.go:103,112,123`.

## Clean files

- `internal/stackrd/infra/managedtiles/stats.go` — pure engine dispatch over a counter read.
- `internal/stackrd/infra/managedtiles/ready.go` — readiness probe dispatch + a `slog` adapter; the only judgement ("engines without a check pass") is explicitly a non-gate.
- `internal/stackrd/infra/managedtiles/s3browse.go` — S3 listing/get/put/delete mechanism (the *credential* concern is filed above against `sqlbrowse.go`, not this file's own logic).
- `internal/stackrd/infra/imagewatch/registry.go` — HTTP manifest/digest mechanism; only `ParseRef`'s identity defaults are filed, at LOW.
- `internal/stackrd/infra/cigate/cigate.go` — the closest to clean of the task files: it reads through the `Deploys` owner and drives the engine, but the two thresholds (`graceNoCI = 3m` "no CI at all, ship it", `timeout = 30m`) and the six `fail(...)` conditions at :65-95 are release policy, so it is **not** listed as clean. Low-medium.
- `internal/stackrd/infra/managedtiles/sqlbrowse.go` helper half (`pgQuoteIdent`, `pgLit`, `pgTable`, `pgError`) — pure string mechanism.
