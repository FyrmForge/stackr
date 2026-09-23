# Business-logic plan — the reconciliation

What this file is: the **joins**. The ten shards under `shards/sweep-*.md` each
read one slice of the tree and found one end of a workflow. This file says
*shard X found one end, shard Y found the other, and here is the resolved
verdict*. It cites shard + finding; it does not restate finding bodies.

Read order: `11-business-logic-sweep.md` (why the sweep exists, the rubric) →
this file (the joins and the waves) → a shard when you need the evidence.

**Status (2026-09-21, after review):** wave 0 and the fix track (§5) are ready
to start. Everything else is ordered and gated — see "At a glance" in §5. Four
product calls are open, listed in §6.

---

## 0. Two rubric refinements the sweep produced

The 6-category rubric in `11-business-logic-sweep.md` is unchanged, but two
layers need a different test applied or the waves mis-sort.

**Config (`internal/stackrd/config/*`) is two tiers, not one.** *(Corrected in
review, 2026-09-21 — G's header says all of config sits below service, and the
first version of this section copied it. The import graph says otherwise.)*

- **Above service — the engines:** `stackconf`, `orgconf`, `envops` import
  `service` (that is what `stackconf.Applier.Ops` holding `*service.TileService`
  *means*). They are **callers**, graded like handlers: they already hold
  services on `Ops`, so a direct store or engine call from them is a bypass
  with a fix available today, not a layering necessity.
- **Below service — the leaves:** `varref`, `settings`, `secrets`, `staging`,
  `envutil`, `runpolicy`, `sharelink`, `envcompare`. Service imports them
  (`sharelink.Links` and `staging.Staged` are interfaces for exactly that
  reason). For these, "it reads the store" is **not** the test. The test is:
  *is the rule a service should own, and does any other surface spell it
  differently?*

G's **misplaced-owner** verdicts rest on caller count (one caller), so they
mostly stand. What the correction changes is **what can call what**, and three
waves depended on the wrong answer: wave 3 (`varref` cannot move up — five
`infra/` packages import it), wave 10 (three audit writers sit below service),
wave 1 (`stackconf` *can* hold `DeployService` — good news). See §2 #5.

**The CLI cannot import `service/` at all.** It is an HTTP client, so
"business logic outside service/" reads differently: what matters is (a) rules
the CLI re-implements that the server owns — drift risk — and (b) rules **only**
the CLI enforces, which are bypassable by any other API client and are
therefore bugs regardless of how clean the CLI code looks.
(`sweep-f-cli.md`, header.)

---

## 1. Cross-shard IOUs

Each shard wrote questions it could not answer. These are the joins, settled.

### Closed

| IOU | Raised by | Answered by | Verdict |
|---|---|---|---|
| Does the stack-level config page carry the twin of `org/config.go:57-72`'s repo-URL normalisation? | B | **C** (`prhook/handler.go:743` finding) | **Yes, and worse — three normalisers.** `service/stack.go:251` and `handler/org/config.go:64` each hand-roll `TrimPrefix("https://github.com/") + TrimSuffix(".git")`; `prhook.normalizeRepo` is a *fourth*, looser one that also understands ssh and bare forms. Every config-repo string comparison elsewhere (`prhook:140,191,301,321`) is a raw `==` that only works because one of the two trimmers ran on the write side. A repo bound by any path that runs neither stops matching, while the tile side still matches it. **One `repo.NormalizeGitURL`** — `store/repo/giturl.go` already exists and J lists it clean, so that is the home. |
| Is `storagetiles.ParseAttachment` a separate rule from `placement.ParseAttachment`? | I | **I → H** | **Alias, not a copy** (`var ParseAttachment = placement.ParseAttachment`). H owns the grammar. `app/storage.go:33-57` (B) is a third *witness*, not a fourth spelling. Count once, in the storage wave. |
| Does the config applier duplicate the panel's backup rules? | (pre-sweep index) | **G + J** | See §2, contradiction #1. |

### Formerly open — all five answered in review (2026-09-21)

Each was a read of a named file. None turned out to be a hole. **Gate B (§5)
is clear.**

| # | Question | Answer | Evidence |
|---|---|---|---|
| **O1** | Is `/dbs/:id/data/*` gated at the route? | **Yes. Trivial.** | `handlers/web/server.go:588-590` mounts `DataUpdate`/`DataInsert`/`DataDelete` through `mutate(…, VerbTileWrite, KindTile, "id")`, and `mutate` (`:240-247`) mounts `RequireAuth` + `middleware.Gate` in the same call. `canWrite` at `db/data.go:110` only hides buttons, which is all it needs to do. |
| **O2** | Does `api/v1` read the build log off `engine.LogPath` itself? | **No — the panel does.** | The API reads through `engine.DeployLog` (`v1/logs.go:116`). The panel hand-rolls `os.ReadFile(h.engine.LogPath(d.ID))` (`deployment/handler.go:179`). `DeployService` gets **one** log method; the panel's hand-rolled read is the category-4 finding. |
| **O3** | Can an API surface reach `AuthService.SaveUser` without the last-admin guard? | **No.** | `SaveUser` has six callers, all in `handlers/web`: `account/handler.go:105,308,347,383` (profile, notifications, appearance, graph prefs — none touch `Role` or `Active`) and `settings/handler.go:548,568` (the two admin toggles). No API route writes a user row; nothing else assigns a user's `Role`/`Active`. **B16 stays a debt fix** (wave 6). |
| **O4** | Does `slugKey` apply to the org canvas? | **No — two meanings, not three.** | The org canvas (`org/graph.go:307`) and home canvas (`:70`) hold no tiles, so there is nothing to translate; they pass raw ids, like the stack canvas (`stackgraph.go:88`). The drift is env canvas (`project/handler.go:986`, applies `slugKey`) vs stack canvas only — fix by picking one. |
| **O5** | Is `githubapp.mergeVerdict` the only CI verdict? | **Yes.** | One definition (`githubapp/ci.go:95`), one consumer (`cigate.go:102-106`). Moves to the CI-gate owner in wave 2, unchanged. |

---

## 2. Where one shard contradicts another

Resolved verdicts. The losing version must be corrected at source or the next
reader picks it up.

**#1 — Backups: neither path is a live bug.** *(Re-corrected in review,
2026-09-21.)*
B filed `settings/handler.go:240-270` (raw `Save` + hand-written
`ReloadBackups`) as the unguarded path. G and J showed otherwise: that path is
the panel's *own database* backup — `repo.BackupStackr`, a row with **no tile**,
which `Create`/`Adopt`/`Reconcile` all bail on (`service/backupschedule.go:289-295`)
— and it calls `backup.Validate` (`:260`) and `ReloadBackups` (`:267`) itself.
Justified. (I's "High — bypassable, the bypass is live" on the same path is
wrong for the same reason.)
G then named **`api/v1/backups.go:205`** as the real hole: raw
`schedules.Remove` where `Delete` exists, "keeps firing until restart". **That
consequence is false** — the very next line, `:207`, is
`a.sched.ReloadBackups(ctx)`. D had it right: `Remove` + a hand-written reload
where `Delete` does both. A category-6 re-spelling, not a live bug.
→ **Verdict: D.** B10 is withdrawn from the bug register. Corrected at source
in `11-business-logic-sweep.md` and `sweep-g-config.md`.

**#2 — `seed.go` is overstated in shard E.**
`sweep-e-boot.md:59-71` says the copy "skips `ValidateResourceHost`, skips
`canonLevel`, skips `checkOwner`, and skips the ACME/proxy resync." Only the
first holds. `seed.go` hardcodes `"instance"` (so `canonLevel` is a no-op for
it) and sets no ACME email (so there is nothing to resync).
→ **Verdict: one skipped rule, not four** — `ValidateResourceHost`, which is the
`installspec.CheckRoot` grammar plus scheme/port/case. `ROOT_DOMAIN=Foo.example.com:8080`
still seeds a row the panel would refuse. Severity **medium**, not high.

**#3 — `ResolveInfraPath` is debt, not HIGH.**
H rates `managedtiles/infrapath.go:28` HIGH on the grounds that it is
`ResolveTarget` with the tenancy check missing. Its own text concedes "caller
today is `config/stackconf/slices.go:29` (a server-side applier, so currently
safe)". One caller, system-scope by design.
→ **Verdict: debt.** It should take a `ResolveScope` or not exist beside
`ResolveTarget`, but it is not a live hole. Do not re-inflate.

**#4 — The cron rule is two rules, not drift.** (H, against a reading C
invited.) `DeployService.deployable:73` refuses cron ("use Run now") — that is a
**surface affordance**. `prhook:424` deliberately deploys cron tiles and
`engine.go:685` has a first-class build-only branch for them — that is a
**domain rule**. Do **not** resolve by moving `deployable` into the engine; that
breaks the push path on purpose-built behaviour. They become two entry points on
one service (`Trigger` vs `Build`/`OnPush`).

**#5 — G's layering premise is half right.** *(Found in review.)* G's header,
and the first version of §0, say all of `config/` sits below `service/`. The
import graph: `stackconf`, `orgconf` and `envops` import `service`; `varref`,
`settings`, `secrets`, `staging`, `envutil`, `runpolicy`, `sharelink` and
`envcompare` do not. → **Verdict: two tiers** (§0). It matters for feasibility,
not for most of G's gradings — see wave 3 and wave 10.

---

## 3. The concept joins

Each heading is one workflow. The table is which shard holds which end. The
verdict is what the wave has to produce.

### 3.1 Deploy — the engine is unguarded and thirteen call sites enter below the service

| End | Shard | What it holds |
|---|---|---|
| The rules | J | `service/deploy.go:63-87` `deployable` — nil, volume, managed, cron, upper-env, unavailable. Filed **clean**. |
| The mechanism | H | `infra/deploy/engine.go:246` holds **one** of those six (`IsVolume`), and `Park:353` holds **none**. |
| The push rule set | C | `prhook/handler.go:424-434` — the whole "is this push deployable" set, in a handler holding `*deploy.Engine` and **no `DeployService`**. |
| The API bypass | A → D | `api/v1/lifecycle.go:75` calls `engine.EnqueueRollback` directly. |
| The config bypass | G / H | `stackconf/apply.go:102,105,107,154`. |
| The mover bypass | H | `volmove.go:691`. |
| The CI bypass | H | `cigate.go:103,112,123`. |

**Verdict.** H's engine-vs-service rule map (`sweep-h-deploy.md`, "Engine-vs-DeployService
rule map") is the authoritative artefact — nothing else in the sweep is that
precise. The shape of the problem: `DeployService` is correct and is not the
door. Thirteen call sites in six packages enter below it (enumerated in wave 1).
The wave adds `Park`, `OnPush`/`Build`,
`Rollback`-for-API and `RecoverStale` to `DeployService`, then repoints the
callers (wave 1 takes ten of the thirteen; prhook's three move in wave 2).

**And "the service is correct" is not quite true — B34.** *(Found in review;
no shard caught it, J filed `deploy.go` clean.)* Three redeploys **inside**
`service/` call plain `engine.Enqueue`, in any environment:
`TileService.redeploy` (`tile.go:158` — reached on a settings save, a build
change and a volume attach/detach), `SliceService.saveAndRedeploy`
(`slice.go:155`) and `DeployService.RedeployIfRunning` itself (`deploy.go:117`
— every variable write). On a git tile `Enqueue` builds the branch head
(`engine.go:629-633`). The config applier knows this is wrong for an upper env
and uses `EnqueueCurrent` instead — whose doc says it exists *"so an apply
never ships code nobody promoted"* (`engine.go:299-302`, `apply.go:85-108`).
The panel and API paths never got that fix. **Setting a variable on a running
service in prod rebuilds it from the branch head**, shipping every commit no
lower rung has run — the exact thing the promotion ladder forbids.

Two sub-joins the map makes visible that no single shard did:

- **Upper-env has three spellings and the enforcing layer is a handler.**
  `service/deploy.go:76` (`envnet.UpperEnv`), `prhook:413` (hand-rolled, admits
  it relies on store order via `isDefaultEnv:454`), engine **absent** —
  and I adds a fourth reader, `components/panel.go:74`, re-deriving the verdict
  to decide what to render. I files `envnet.UpperEnv` itself as High.
- **"Is this deployment still live" has five copies.** Four were collapsed into
  `internal/deploystate` (E files that package as the model to copy);
  `volmove.go:737` hand-writes a fifth (H). `service/deploy.go:126` records that
  the four had already drifted once.

### 3.2 Variables and secrets — the read half has no owner at all

This is the sweep's headline structural finding and it spans six shards.

| End | Shard | What it holds |
|---|---|---|
| Every write | J / G | `service/variable.go` — filed **clean** by J. |
| Every *resolved read* | G | `config/varref` — the env→stack→org precedence walk (`varref.go:520`). `VariableService` owns **no** read path but a bare `List` passthrough (`:325`). |
| Handlers reaching past the service | G | `project/handler.go:1403`, `api/v1/variables.go:158-165,321`, `app/handler.go:945`, `org/graph.go:547`, `project/stackgraph.go:453,507`. |
| The name regex, copy 3 | B | `org/config.go:162-167`, `org/settings.go:384`. |
| The name regex, the *other* grammar | G | `stackconf.go:1170` `envVarRe` — strictly tighter. |
| A fourth resolver | G | `orgconf/storage.go:73` — byte-identical `refRe`, own lookup, ignores scope and bucket. |
| The `${{ }}` spelling, client side | F | `cli/lifecycle.go:76` and `cli/cmd/vars.go:174` — two CLI spellings that already disagree with each other, neither checked against `varref`. |
| The store fighting the service | J | `sqlite/tiles.go:76-88` `projectEnvVars` — every `UpdateTile` re-projects the env blob into `variables` rows. `VariableService.syncBlob` (`variable.go:236`) exists **solely to fight it**. |
| The parked-tile release | H | `deploy/waiting.go:15,72,94,113` — `ClearWaiting`/`ClearWaitingEnv`/`ClearWaitingOrg`, three near-identical functions whose only difference is scope. |
| The audit of a secret write | J / H | `managedtiles.go:427-445` `PublishConnection` records an audit row only on a real change — six call sites for that one invariant. |

**Verdict.** *(Rewritten in review — the first version moved the walk into
`VariableService`, which cannot work.)* `varref` is imported by five packages
**below** service — `infra/deploy` (`engine.go`, `waiting.go`), `infra/jobs`,
`infra/managedtiles`, `infra/storagetiles`, `infra/proxy` — and the deploy
engine resolves a tile's environment through it at build time. `service/`
imports `infra/deploy`, so the walk cannot move up without a cycle.

So the walk **stays in `config/varref`**, the one package every consumer can
reach. The wave makes it the *only* walk: `runner.go:773 checkRef` (the plan
gate — this is B5) and `orgconf/storage.go:73` delegate to it instead of
re-implementing it, and the handlers above stop importing `varref` and ask
`VariableService` for a read door (`Lookup`) that calls it. (`app/panel.templ`
imports it too — a `.templ` source, so Q3.)
`VariableService` owns the door; `varref` owns the walk.

### 3.3 The work queue — half the table has an owner

H and J found the same thing from opposite directions and neither could see the
whole without the other.

- **J:** `WorkItemService` (`service/workitem.go:39-67`) is a self-declared
  forwarder with six writes and **zero reads**.
- **H:** every read goes straight to `repo.Store` —
  `workqueue.go:216,250,274,337`, `jobs.go:281`, `volmove.go:192,346,355`,
  `backup/work.go:145`.
- **J again:** the only service-layer reader is `PlanService.Work`
  (`plan.go:207`), which reaches **past** `WorkItemService` because it has no
  reads.
- **H's sharp edge:** two of those reads are **eligibility**, not UI —
  "is a move already in flight for this tile" (`volmove.go:346`) and "is this
  run still stoppable" (`jobs.go:281`).

**Verdict.** `WorkItemService` gains `Get`/`ListByStatus`/`Latest`, and the two
eligibility questions become named methods. H's own line is the one to keep:
*"the guard tests could never have seen this."*

### 3.4 Storage and volumes — no `VolumeService` exists, and `StorageService` owns neither end of its own delete

| End | Shard | What it holds |
|---|---|---|
| Probe, ×4 | I | `storagetiles.Probe` called from `service/storage.go:110` **and** `:216`, `server/storage.go:33`, `api/v1/lifecycle.go:234`. (The pre-sweep index said 3; I counts the service's own double.) |
| Probe *policy* | I | When to probe and what a failure means is a rule; mount-and-list is mechanism. Split. |
| Delete's other half | J | `StorageService.DeletePath` is deliberately hollow, but `clus.RemoveVolume(repo.StorageVolume(p.ID))` is spelled **twice outside it** — `server/storage.go:140`, `api/v1/storage.go:184`. That is not a confirmation, it is the delete. |
| Volume create/remove | B | `server/handler.go:271-320` — name validation, node resolution, confirm-against-live-list, direct `h.clus`. |
| Volume attach | B | `app/handler.go:682-707` — a fifth rule set; the absolute-path check at `:694` only runs when `targetID != ""`. |
| Volume read/write | C | `filebrowse/filebrowse.go:39,126-166` — `CleanDir` confinement plus a direct `*cluster.Cluster` backend carrying the node rule. |
| "Which volumes does this tile hold" | H | `volmove.go:243-268` — and `placement.IsPinned:78-92` is a **fourth** reading, `placement.NodeOf:48-76` a **fifth**. H records two past data-loss incidents caused by these disagreeing. |
| The row rules | J | Scattered inside `TileService` (`validateVolume`, `checkAttach`, `orphanVolumes`), and `repo.ValidVolumeName` sits in `models.go` "because nothing above it would own it". |
| Local-backed rule | I | `storagetiles.ValidateAttach` — a corruption-class rule enforced directly from `app/storage.go:51`. |
| Org share requires `export` | G | Config requires it; `service/storage.go:78` requires it only for a *local* pool. |
| Drop-then-save contract | I | `DropOrgShareVolumes` — "the caller must not save the edit if this fails" is written **only in a doc comment**, and three callers each honour it on their own. |

**Verdict.** Two services, both missing. `VolumeService` does not exist at all.
`StorageService` exists but owns neither the probe policy nor the volume half of
its own delete. The five readings of "does this tile hold data" are the highest
data-loss risk in the sweep.

### 3.5 Nodes — the seam runs through one handler line

I produced the full split table (`sweep-i-infra.md`, "Node lifecycle"). The
shape: **everything before a node has joined is infra's; everything after is the
service's.** `infra/nodes` owns who may join, under what name, with what key,
for how long; `NodeService` owns drain, remove and the pinned-tile refusal.

The join B supplies: `handlers/web/handler/server/nodes.go:67` calls
`h.nodes.AddNode` and then `service.NodeService.EnsureAgent` as two steps of one
operator action — **the handler is currently the only place the whole "add a
node" workflow exists.** E supplies the third fragment: `main.go:562-572` gates
`EnsureAgent` on `rt.ListNodes() >= 2` at boot, a gate the panel's path does not
apply, so a single-node box adding its second node gets a different answer than
a reboot does.

`service/node.go`'s own comment says "there is no rule on the far side of these"
about its `Adopt`/`IssueKey`/`BurnKey` block. True of the writes; false of the
decisions that produce them.

**And the seam is currently a boot panic — B0.** `nodes.Service` takes `rows`
at `main.go:584` and `Sync` runs at `:590`, but `rows.Nodes` is not filled until
`:680`. Every write `Sync` makes — `mirror` → `Rows.SaveServer`, adoption →
`Rows.Adopt` — goes through a nil `*NodeService`. There are also **two**
`NodeService` instances built from identical arguments (`:560`, `:680`), so the
service this wave is meant to consolidate is already two objects. No shard found
this: E read `main.go` for *rules* and correctly filed the wiring as wiring;
I mapped the split but not the construction order. It only shows up when the
node join is read as one workflow across both.

### 3.6 Webhooks and CI — the worst single file, and it cannot move first

C: `prhook/handler.go` is 784 lines, one struct holding a store, `*deploy.Engine`,
an `envops.Ops`, a `stackconf.Applier`, a githubapp client, an `orgconf.Runner`,
a workqueue and seven services — and essentially the whole "what does a git push
do" workflow. There is no `WebhookService` and no `PREnvironmentService`.

Joins:

- **C → H:** it cannot move onto `DeployService` until `DeployService` has
  `Park` and `OnPush`. H: `engine.Park:353` has **zero** checks and `DeployService`
  has no equivalent. → **ordering constraint, see §5.**
- **C alone:** `watchMatch` (`prhook/watch.go:13`) — the whole watch-paths
  language — has exactly one caller and one test, both inside `prhook`. The
  API's deploy path, the CI gate and imagewatch all deploy **without it**.
  → prhook also cannot go last: it is the sole home of a rule three other
  surfaces should honour.
- **C → G:** `prhook:734` calls `envops.Ops.Teardown` directly and `:567` calls
  `envops.CopyLayout` — a handler reaching past `EnvironmentService` into the
  layer below it. G independently flags the same bypass from the config side.
- **I → H (O5, answered):** `mergeVerdict` defines CI pass/fail in `githubapp`;
  `cigate` holds the thresholds. Two packages, one gate — and one definition
  with one consumer, so it moves unchanged.
- **C → C:** `Hook` and `HookConnector` disagree about which stacks a delivery
  addresses (`:139-146` vs `:191-197`). Same repo, two answers.

### 3.7 Config plans and the apply gate

- **D:** six spellings of "not bound to a config repo" across two API files;
  `cp.Status != "pending"` re-spelled in the org handler where the **stack** side
  already delegates to `PlanService` (`config.go:169-196`, proved by
  `config_test.go:TestDecideRejectsNonPendingPlan`). Asymmetry **inside** one
  surface.
- **B:** a **third**, wider definition — `org/plans.go:130` counts `pending` *or*
  `error` as waiting, so the banner calls an errored plan actionable and the
  approve route 409s on it.
- **C:** `prhook:370-383` `maybeAutoApply` adds a fourth, and returns silently
  when `h.work == nil` — a mis-wired binary deploys nothing and logs nothing.
- **G:** the consequential half — `apply.go:223` `PolicyFor` plus `holdManual`,
  `allAuto` and `ApplyPlan:350` decide **whether a webhook may change production
  unattended**. G rates the `ApplyPlan` ordering as *correctly placed* (it is the
  config engine) with the caveat that it reaches **through** `Ops` into six
  services rather than being called by one. Do not move it casually.
- **J:** `sqlite/stacks.go:264-269` — `status IN ('pending','clean','error')`
  is the policy statement for "undecided", eight lines of comment arguing for
  it, in SQL. `PlanService.SupersedePending` forwards blind.

**Verdict.** One `Waiting()` predicate on `repo.ConfigPlan`, one status gate in
`PlanService`. `PolicyFor` stays in config but gets read by every surface that
answers "will this apply on its own?".

### 3.8 Authorization — point 18's "one path", implemented twice, plus its outriders

D found the core: `v1/auth.go:442-491` and `middleware/orgctx.go:387-476` are
the same four-branch switch, **already** diverging on 404-vs-403. The outriders
are spread across four shards and none of them is visible from the core:

| Outrider | Shard | What |
|---|---|---|
| `cmd/stackrd/ws.go:17-47` | E | The websocket room→permission map, in `package main`, on the raw store. Fails closed only by accident of the `switch` default, and unreachable from any test that does not build `main`. **High.** |
| `setuppending.go:52-62` | C | `setupOwner` reads `GetOrgMember` for *the org the request addressed* — the active-org-vs-resource-org distinction `routelevel_test.go` keeps a whole column for. |
| `account/handler.go:196-258` | B | The API-key grant ceiling (`CanGrantWrite`) lives in a **page package**, and `cli/handler.go:63,118` re-spell it inline rather than calling it. |
| `org/settings.go:203-205` | B | `ownedSettingsOrg` is a bare alias for `settingsOrg` whose name and doc comment claim an owner check. ~15 call sites read as owner-gated. The check really moved to the route verbs (`server.go:395-432`), so this is a **false doc comment**, not a hole. Fix = delete the alias. |
| `managedtiles/resolve.go:60-78` | H | `ResolveScope.AllowedOrgs` — a nil value silently means "everything", so a forgotten field is a tenancy leak rather than an error. Both request-driven callers do pass it today: **misplaced, not bypassable.** |
| `middleware/orgctx.go:151-285` | D | A second role vocabulary (string comparison) beside `AccessService` levels, **in the same file** that uses the levels at `:469-471`. |
| **The five `KindDeferred` routes** | D | *(Added in review.)* The route gate checks **nothing** on these (`orgctx.go:403-405`); the handler's own check is the only one. `POST /api/v1/stacks` (`v1.go:404` → `orgForCreate`, `v1/helpers.go:29-100`), `/resolve` (`:499`), `/domain-resources` (`:519` → `resolveResourceTenancy`), `/backup-destinations` (`:529`), and the panel's `POST /projects` (`server.go:506`). Correct today. Moving any of them without a test is how one opens. |
| `account/handler.go:211-233` | B | Key revoke: "owner or admin" decided by listing **every key on the server** and filtering in Go. The service has no owner-scoped read, so every caller must remember the filter. |

J files `service/access.go`, `gate.go` and `tenancy.go` **clean**. The target
exists; the surfaces above do not use it.

### 3.9 Graph and canvas

- **A:** the `slugKey` drift (`handler.go:986` vs `stackgraph.go:88`) — same
  service method, two node-key meanings. **Drift that already happened.**
- **J:** and the shape validator is one layer *below* the service, in
  `sqlite/infra.go:245`, so `GraphService` owns **neither half**.
- **J → B (O4, answered):** the org canvas is not a third meaning — it holds
  no tiles, so there is nothing to translate (§1).
- **Notifier fan-out, counted across three shards:** A finds 12 sites in
  `project/`, B finds 6 more in `org/graph.go` — and notes `org/graph.go:97,105,142,150`
  **deliberately omit it**, an asymmetry invisible at the call site. C adds
  `prhook:338,355`. Repo-wide 22 sites sit outside `service/`. Four services
  already notify from inside their own writes, so the pattern exists.
- **C:** `graph.WorstStatus` is held in step with `canvas.templ` and
  `frontend/static/js/graph.js` **by a comment**.
- **B:** `search/handler.go:314-357` is a fifth spelling of the canvas shape
  rule and silently `continue`s past any tile it classifies differently from the
  canvas it links to.

### 3.10 Managed instances and slices

- **H:** `managedtiles` is a parallel deploy pipeline. `DeployService.deployable:70`
  **refuses** managed tiles ("a managed database is not deployed; it is
  provisioned") while `managedtiles.Deploy:476` deploys them. *The service
  refuses the word and a different package does the thing.*
- **H:** the public-slice gate is spelled **three** times
  (`provision.go:35`, `provision_s3.go:31`, `:212-218`) — the create path and the
  flip path each carry a copy.
- **H:** `resources.go:155-183` — infra **edits a consumer tile's `Env` text**
  (`t.Env = strings.Join(kept, "\n")`) on detach.
- **G:** `orgconf.go:1006-1016` builds an org-scoped instance row by hand and
  calls `CreateTile` with **no `Tiles.Validate`** — the one tile-create path in
  the repo that skips the shared validator, while the stack applier at
  `apply.go:1253` does call it.
- **A:** `db/data.go:58-96` assembles `managedtiles.PGCreds` **in a handler**.
- **D:** `provisionApp`/`attachProvision` call Provision **and** Wire as two
  service calls with no transaction — a failure between them leaves an unwired
  slice.
- **J:** `env_intended` is owned by `ManagedInstanceService` and shouldn't be —
  an `Intended` row is `(environment_id, tile_slug, key, value)` with no managed
  instance in it.

### 3.11 Settings

- **G:** `config/settings/settings.go` carries real cascade rules (`Resolve`,
  `Check`, `Merge`, `Levels`) — and `settings.Merge` has **exactly one caller
  repo-wide**, `service/settings.go:46`, immediately followed by `Check()`.
  **No drift.** The finding is purely altitude: a `url.Values` form binder two
  layers below the only form that uses it.
- **D:** the *catalogue* is the drift. Five hand-written lists that disagree per
  level — `v1/settings.go:192-255` (14 knobs, mounted at all four levels by
  `v1.go:367-374`) vs four `.templ` files offering 10/4/3/3. So
  `PATCH /api/v1/stacks/{id}/settings` publishes and accepts `build_node`,
  `node_group`, `metric_retention_hours`, `protect_password` and four
  concurrency knobs at stack and env level, **where the panel offers none.**
- **E:** `seed.go:25-66` is a sixth writer, in `package main`.
- **J:** `SettingsService.Value`/`SetValue` are untyped passthroughs with no key
  validation and no resync, and four other services write the same table around
  them.

**Verdict.** `config/settings` keeps the cascade and gains the catalogue; every
surface enumerates it instead of writing a list. The `.templ` half is scope
question **Q3** below.

### 3.12 Registry

I found the tenant boundary: `registry.Namespace` (`token.go:201`) — an org owns
the prefix `<org-slug>_` and nothing outside it, and **the trailing underscore
is what stops `acme_` matching `acme-evil_`.** Four surfaces build that prefix
themselves (`service/registry.go:250`, `api/v1/registry.go:105`,
`org/registry.go:85`, plus a generated templ as spread evidence).

D found the write side: the API re-implements **three** writes the panel gets
from the service — `AddExternal` (`v1/registry.go:216`), the managed-delete
refusal (`:275`, *the exact bug the panel was already fixed out of*), and a
fourth shape for setting the registry domain (`:233-267`) that the panel has no
equivalent for at all.

B found the read side: `org/registry.go:123-152` `liveTags` is the buggy matcher
the delete path was fixed to stop using — it indexes by `strings.Cut(d.ImageTag, "/")`
and drops any tag stored without a pull host, so the Images table's "runs on"
column still carries the defect `:201-205` documents as fixed.

### 3.13 Orgs, members, invites

- **B:** invite liveness (`!UsedAt.Valid && now < ExpiresAt`) is spelled **four**
  times — `auth/invite/handler.go:36`, `org/members.go:363`, `:422`, `:379` —
  and `GetInvite` returns dead invites to every caller.
- **B:** `auth/invite/handler.go:129` — `_ = h.members.UseInvite(...)` discards
  the error. A join that lands with a failed burn leaves a **single-use invite
  reusable**, and nothing logs it. *(Worse than this — the burn is not
  conditional either. B14.)*
- **D:** `v1/members.go:117-132` mints invites through the raw
  `members.CreateInvite` hatch — no expiry upper bound, no already-a-member
  check — while `v1/members.go:45-63` on the *same surface* calls `Invite` and
  gets both. `slugpath_test.go:TestUnknownRoleIsRefused` asserts an unbounded
  expiry must be refused — against the service, not this route. *(B15.)*
- **B + D agree:** the role whitelist is folded to `"member"` **before** the
  service can refuse it, on both surfaces (`org/members.go:227,315`,
  `v1/members.go:178`). The service's whitelist never fires from anywhere.
  **Shared defect, not a divergence** — D's framing is the correct one.
- **B:** org **rename** carries a second, opposite-direction anti-squat rule
  (`org/members.go:88-117`) beside `service.CheckOrgSquat`, and only the
  handler's protects rename. G confirms from the config side that the
  registry-image gate **is** correctly shared and that there is no API
  org-rename route at all.
- **J:** `OrgService.Delete` is hollow; the four rules are in
  `org/members.go:171-203`. One caller, no API route — **misplaced, not
  bypassable.**

### 3.14 The store is deciding things

J's section "Business logic in `store/repo/sqlite`" is the shard nothing else
could have produced, and it changes the shape of the extraction: several rules
are not in a handler, they are *below* the service. The joins that matter:

- `sqlite/tiles.go:31-42` fills four defaults (`ScopeKind`, `Replicas`,
  `EndpointProtocol`, `UpdatePolicy`) that `TileService.applyDefaults` does
  **not** — so a caller reading its own struct back does not see them.
- `sqlite/tiles.go:76-88` `projectEnvVars` is the tile→variables mirror running
  inside the tile writer; §3.2.
- `sqlite/backups.go:90-95` `UpdateBackup` — `kind` is **not** in the `SET` list
  while `BackupScheduleService.Reconcile:147` assigns it and `normalise`
  validates it. Accepted, validated, discarded. J notes this is the same failure
  `sqlite/tiles.go:184-200` documents at length for the old whole-row
  `UpdateTile`, reproduced on a table that never got the fix.
- `sqlite/stacks.go:302-306` — `ORDER BY type = 'ephemeral', position, created_at`
  **is** the env ladder, and `EnvironmentService.ListForStack` documents the
  order as load-bearing and cannot enforce it. This is the same ladder
  `prhook:454` `isDefaultEnv` hand-rolls (C) and `envnet.UpperEnv` answers from
  the other end (I). **Three readings of one ordering, one of them a `WHERE`
  clause.**
- `sqlite/deployments.go:55-68` `SweepStaleRuns` — the interrupted-run policy
  in a `[]string` of SQL, reached from `main.go:365` (E) with no service between.

### 3.15 Audit

J: **13** write sites outside `service/` (not 14 — inclusion rules stated in the
shard). D adds the join B and D each half-saw: the panel and the API use **two
actor formats** (`u.Email` vs `"api:"+key.Name`) and two sets of call sites, and
both fire from the caller rather than from the service that reads the value.

J's warning is the one to carry: **the actor vocabulary is already forked five
ways** — bare email, `api:<key>`, `system:<what>`, `config:<slug>`,
`<kind>-link:<id>`. An `AuditService.Record` must take that as a typed
parameter or the five spellings become five string literals through one door.
And `AuditService`'s own doc comment currently **argues against** the method;
rewrite it when the method lands.

---

## 4. Bug register — deduped across all ten shards

Live defects, independent of the refactor. "Verified" = I read the cited lines
myself (in the reconciliation or the 2026-09-21 review). Agent-only rows are
**claims**, not facts.

| # | What | Where | Verified |
|---|---|---|---|
| B0 | **Boot panic.** `nodeSvc.Sync(ctx)` runs at `main.go:590`; `rows.Nodes` is not assigned until `main.go:680`. `Sync` → `match` → `mirror` → `Rows.SaveServer` → `r.Nodes.Save` on a **nil `*NodeService`** → nil deref. Masked by `mirror`'s no-change early return (`nodes.go:228-231`), so it only fires when swarm state differs from the row — which is exactly the case the comment at `main.go:586-589` says the call exists to handle ("Sync writes the manager's own swarm id onto its servers row. Nothing else does"). The hand-joined-node adoption path (`nodes.go:152` `Rows.Adopt`) is the same nil through a second door. **Also: two `NodeService` instances are built from identical arguments** — `nodeLifecycle` at `:560` and an anonymous one at `:680` — so "the node service" is two objects and neither is the whole owner. | `cmd/stackrd/main.go:584,590,680` | **me** |
| B1 | `env rm -y` / `env reset -y` force-destroys running tiles; the server's refusal is unreachable from the CLI. `infra.go:233-256` already fixes this exact shape. | `cli/cmd/promote.go:240,274` | **me** |
| B2 | API rollback skips all five deployability rules, including the upper-env ladder. | `api/v1/lifecycle.go:75` | **me** |
| B3 | Backup `kind` change accepted, validated, discarded. | `sqlite/backups.go:90` | **me** |
| B4 | Plain write silently declassifies an existing secret. Plan: `13-secret-declassification.md`. | `service/variable.go:195` | **me** |
| B5 | `checkRef` never reads the env rung → a file declaring `environments.<env>.vars` **cannot apply**, and the plan's own guidance text contradicts its own gate in adjacent functions. | `stackconf/runner.go:773` | agent (G) |
| B6 | `StackRef.Protected` is parsed (`:125`) and read nowhere. Its doc says removing the stack from the file "is refused outright instead of planned" (`:92-93`); the delete loop never checks it (`:789-810`) and its own comment says why — the flag lives in the stanza being removed, so "absence means the operator already removed the flag too; the manual approve is the gate". A safety flag that cannot work as written. Distinct from the live `EnvConf.Protected`; the name collision makes the dead one look wired. | `orgconf/orgconf.go:92-94,789-810` | **me** |
| B7 | `syncBindings` grants and never revokes; its doc comment says it revokes (`:172-174`). A tile that stops referencing a slice keeps its binding, and `varref` keeps resolving it. *(Citation fix: `DeleteBinding` **is** called — by `ManagedInstanceService`'s detach, `service/managedinstance.go:505` — just never from here. The fix is not one line: nothing records which bindings a reference created and which a panel attach did.)* | `stackconf/slices.go:179-203` | **me** |
| B8 | **Downgraded to low.** Org-setup writes `<org-slug>.<base-host>` through raw `Save` with no `HostTaken`. But `host` is `UNIQUE` in the schema (`001_initial.up.sql:390`), so a collision fails the insert instead of double-claiming, and a host under your *own* slug cannot squat anyone. Real cost: an unfriendly error at "finish setup". | `org/setup.go:207-226` | **me** |
| B9 | The org applier is the only tile-create path that skips `Tiles.Validate`. | `orgconf/orgconf.go:1006` | agent (G) |
| ~~B10~~ | **Withdrawn — not a bug.** Claimed: an API-deleted schedule keeps firing until restart. The next line, `:207`, reloads the scheduler. It is a hand-written `Remove` + reload where `Delete` does both (category 6). §2 #1. | `api/v1/backups.go:205-207` | **me** |
| B11 | CI-gated tile deploys **unparked** in its PR env — `syncPR` ignores `WaitForCI`. | `prhook/handler.go:719` | agent (C) |
| B12 | A config-managed stack whose tiles clone a different repo gets PR envs through `/hooks/github/:stack` and not through `/hooks/connectors/:id`. | `prhook:139-146` vs `:191-197` | agent (C) |
| B13 | PR-env close calls `envops.Ops.Teardown` + `sched.Reload` directly, skipping `EnvironmentService.teardown`'s cleanup of staged changes and canvas positions (`environment.go:220-236`). Every closed PR leaves orphan rows, and the stack-wide staged listing shows edits to tiles that no longer exist. **Not security, not data loss** — skipping the gate and the running-check is deliberate for a PR close. | `prhook/handler.go:726-739` | **me** |
| B14 | **Worse than filed.** Single-use invites are not single-use. The burn is `UPDATE invites SET used_at = … WHERE id = ?` — no `AND used_at IS NULL`, no rows-affected check (`sqlite/members.go:59-63`) — it runs **after** the join, and its error is discarded (`_ =`). Two people opening one open (no-email) invite at once both join; a failed burn leaves it reusable for anyone. Open invites can only be minted through B15's path today, so fixing B15 narrows this to email-bound invites, where the address check and an idempotent `Join` limit the damage. Fix both anyway. | `auth/invite/handler.go:124-129` | **me** |
| B15 | API `createInvite` → `newInvite` → raw `members.CreateInvite`: no expiry cap (`InviteDaysMax`), no already-a-member check, role folded, and an **empty email is accepted** — an open, anyone-with-the-link invite. `addMember` on the same surface (`:45-63`) calls `MemberService.Invite` and gets all of it. Compounds B14. *(The test D cited, `TestUnknownRoleIsRefused`, calls the service directly — it asserts the rule, not this route.)* | `v1/members.go:117-167` | **me** |
| B16 | Two admin guards contradict: demote is allowed while 2+ admins exist; disable is refused for **every** admin (`:562`). Read-then-write, no transaction. Not API-reachable (O3), so debt, not a hole. | `settings/handler.go:521-575` | **me** |
| B17 | `liveTags` is the buggy matcher the delete path was fixed to stop using. | `org/registry.go:123` | agent (B) |
| B18 | **Not reproduced — the comment is stale.** The package header says a link's scope "binds against whatever org the cookie held". Today mint is gated on the org of the stack in the URL (`server.go:533` → `orgctx.go:407-408` → `tenancy.go:96`), and the link's owner is that same stack (`project/handler.go:2194-2196`). The header predates point 18's resource-org gate. Fix = delete the sentence. | `sharelink/sharelink.go:40-44` | **me** |
| B19 | API re-implements three registry writes the panel gets from the service, incl. the managed-delete refusal the panel was already fixed out of. | `v1/registry.go:216,233,275` | agent (D) |
| B20 | API release listing answers "which plan blocks this promote" by exact-SHA only; the panel is env-scope and commit-ordering aware. A CI gate and a person see different answers. | `v1/releases.go:35-120` | agent (A+D) |
| B21 | **Milder than filed.** `stackr tile set --memory 512` sends `cpu: 0` for the half not given — and `0` means *inherit the cascade* (`settings.go:151-159`), not zero CPU. So it silently wipes the tile's own cpu override. `--dockerfile` alone does the same to `build.context`, two lines below a comment saying sending one half clears the other. Not security, not data loss. | `cli/cmd/tile.go:246-263,309-314` | **me** |
| B22 | `stackr tile create web --schedule ...` blanks schedule/command/timeout client-side when no `--cron`/`--function` is given, so the server never sees them: a schedule-less service, reported as success. The server would have refused. | `cli/cmd/tile.go:126-129` | **me** |
| B23 | Org share with no `export` accepted over the API, fails later at probe. | `service/storage.go:78` vs `orgconf/storage.go:53` | agent (G) |
| B24 | `atoiOr` folds a typo to "not set" on tile create; `formInt` returns `-1` so the service refuses, and its comment names the bug. The fix landed on one side only. | `project/handler.go:1561` vs `db/handler.go:468` | agent (A) |
| B25 | A tile deploy launched detached on `context.Background()`; failure reaches nobody and there is no row to poll. | `db/handler.go:272-276` | agent (A) |
| B26 | Three tile-create whitelists across three surfaces; volume→image forcing exists only in the web handler. | `project/handler.go:445-517`, `v1/apps.go:67`, `v1/volumes.go:50` | agent (A+D) |
| B27 | `server/nodes.go:357` swallows `placement.For`'s error and falls back to the raw column, offering the wrong move targets exactly when placement is unavailable. | `server/nodes.go:349-371` | agent (B) |
| B28 | `wireProvision` decides whether the app gets its connection string **at all**, in a handler `if`. | `app/handler.go:806-839` | agent (B) |
| B29 | Redeploy pre-gate differs from the service gate it claims to delegate to, in the same function; the sibling caller applies no pre-gate at all. | `app/handler.go:1098` vs `:736` | agent (B) |
| B30 | Five config-managed refusal spellings with three messages; the most consequential (`uiManaged` at `:811`) is the furthest from the gate. | `app/handler.go:1109-1158` | agent (B) |
| B31 | Nil-vs-false HTTPS: three hand-written copies of the nil convention; the comment records that a previous version shipped a domain redirecting HTTP onto TLS nobody served. | `app/handler.go:1474-1486,1539-1550` | agent (B) |
| B32 | `refBroken` decides whether to heal a tile by **string-matching** an error message produced in another package. Reword `varref.go:590` and self-healing silently stops. | `stackconf/apply.go:1163` | agent (G) |
| B33 | `orgconf.Apply`'s retry branch string-matches `"no middleware "` against `middlewares.go:177`'s wording. Same class as B32. | `orgconf/orgconf.go:777` | agent (G) |
| **B34** | **Incidental redeploys skip the promotion ladder.** A variable write, a tile settings save, a volume attach and a slice provision all redeploy through plain `engine.Enqueue`, which builds a git tile from its branch head — in **any** env. In prod that ships commits no lower rung has run. The config applier uses `EnqueueCurrent` for exactly this reason; nothing else does. §3.1. *(Found in review; no shard.)* | `service/deploy.go:117`, `service/tile.go:158`, `service/slice.go:155` | **me** |
| **B35** | **`stackr vars set` fails next to any secret, and the error gives advice that deletes it.** The CLI reads the whole set, overlays, and PUTs it back (`cli/client.go:365-393`). Without `secrets:read` the secrets come back masked, `write` refuses a masked value (`service/variable.go:161-164`), so the whole set is refused. The refusal says *"omit it to keep the stored value"* — true for `Set`, but the API's only verb is PUT → `Replace`, which **deletes** anything omitted (`:100-118`). Plus F's lost-update race: two writers doing read-modify-write drop each other's rows. | `cli/client.go:365`, `service/variable.go:164` | **me** |
| B36 | The API-key grant ceiling ("a key never carries more than its minter holds") lives in a page package and is spelled three times; `cli/handler.go:63,118` re-spell it inline and import `account` for the fourth (`MintOrg`). | `account/handler.go:196-258` | agent (B) |
| B37 | Secret redaction happens **after** the values are loaded, in the handler; `OrgVarValue` (`:316-326`) serves plaintext leaning entirely on the route verb. One forgotten branch from a leak. | `org/settings.go:264-272` | agent (B) |
| B38 | The S3 browser always connects with **root** credentials (`s3ClientFor:20`); bucket scoping rests entirely on the handler passing the right bucket name. The PG browser is scoped by credentials; this one is not. | `managedtiles/s3browse.go` | agent (H) |

Two more that are **dead code carrying a live-looking rule** rather than
defects, listed because the next reader cannot tell which copy is live: D's
eight-row dead-code table (`v1/helpers.go:124`, `v1/domainresources.go:146`,
`v1/backups.go:164`, `v1/envs.go:39`, `:136`, `v1/auth.go:409`, …) and G's
`varref.EnvLines` (zero callers — *either a caller lost it, in which case
somebody is starting containers with unreachable hostnames, or it should go*).

**Verify before acting on any agent-only row.** The review pass (2026-09-21)
read the eight security/data-loss rows plus B10 and B22. Of those ten: four
held (B7, B15, B16, B22), one was **worse** than filed (B14), two were milder
(B13, B21), one was a stale comment (B18), one was **false** (B10), and one is
already caught by a schema constraint (B8). B6 was read afterwards and held.
That is the base rate for the 21 agent-only rows left (18 original, B36–B38
added in review).

### Where each bug gets fixed

*(Added in review.)* The first version of this plan gave 26 of 34 rows no
home: no wave's *Done* or *Test* named them, and orgs, registry and the CLI
had no wave at all. A gate that blocks on bugs nothing repairs is a stall.

| Home | Rows |
|---|---|
| **Fix track** (§5) — live, verified, small; own commit each, no wave needed | B1, B3, B4, B14, B15, B18, B21, B22, B34, B35 *(the error message only)* |
| Wave 0 | B0 |
| Wave 1 — deploy | B2, B29 *(and absorbs B34's helper)* |
| Wave 2 — prhook | B11, B12, B13 |
| Wave 3 — variables | B5, B7, B32, B35 *(the merge verb)*, B37 *(and keeps B4's tests)* |
| Wave 5 — storage | B23, B26, B27 |
| Wave 6 — authorization | B16, B36 |
| Wave 9 — store rules | *(keeps B3's round-trip test)* |
| Wave 11 — ride-alongs | a registry: B17, B19 · b orgs: B8 · c managed/slices: B9, B25, B28, B38 · d config plans: B20, B33 · f app handler: B24, B30, B31 |
| **Needs darhvader** | B6 — waits on Q6 (§6). |
| Withdrawn | B10 |

---

## 5. Waves

### The two open questions from `10-total-extraction.md`, now answerable

**Q1 — where does prhook go?** Neither first nor last.

- It **cannot go first**: it needs `DeployService.Park` and an `OnPush`/`Build`
  entry point to exist, and today neither does (H: `engine.Park:353` has zero
  checks; C: the handler holds `*deploy.Engine` and no `DeployService`).
- It **cannot go last**: it is the sole home of `watchMatch`, a rule three other
  deploy paths should honour and none does, and B11/B12 are live.

→ **Sequence: deploy wave first** (`DeployService` gains `Park`, `OnPush`/`Build`,
`Rollback`, `RecoverStale`), **prhook wave second**, repointing onto them and
lifting `watchMatch` out.

**Q2 — guard scope?** Allowlist the *helpers*, not the file.

C argues the `mutate`/`read` pair in `server.go` is a good pattern that cannot
be split — declaring a verb and mounting its check are one act. So the
replacement guard allowlists `mutate`/`read`/`skipOnPoll`/`RegisterStaticPages`
and the DI construction block **specifically**, not `server.go` wholesale —
because C also found two real decisions inside it: first-boot decided twice
(`:789-800`, `:808-819`) and `tilePage`/`redirectTile` (`:871-941`) re-walking
a resolver `AccessService` already owns.

### What the deleted guards actually missed

Stated in full, because the whole sweep exists as a consequence and the
replacement guard will inherit the same holes if they are not written down.

The three guards (`guard/readfree_test.go`, `writefree_test.go`,
`handlers/web/storefree_test.go`) derived their method set from the
`repo.Store` **interface declaration** and then matched call sites **by method
name**, walking from the repo root (`readfree_test.go:71`, `root := "../../.."`).

**Not a false negative — a concrete `*sqlite.Store` receiver.** Because the
match is by name, `store := sqlite.NewStore(database)` (`main.go:208`) was
counted like any other. `cmd/stackrd` was in scope and four of its calls sat on
the `stillReading` worklist by name (`GetServer`, `GetStack`, `GetUserByID`,
`ListDomainResources`). A reviewer suggested these bypassed the guard; they did
not.

**The real false negatives, in order of how much they cost:**

1. **Any receiver that is not a `Store`.** `h.clus.RemoveVolume(...)`,
   `h.nodes.AddNode(...)`, `h.rt.ListNodes(...)`, `engine.EnqueueRollback(...)`
   touch no store method, so they passed all three guards while owning the rule
   outright. This is the hole the whole sweep was run to fill — §3.1, §3.4, §3.5
   are all invisible to a store-shaped guard.
2. **The allowlist itself.** `stillReading` is a worklist that "shrinks; it does
   not grow" — but every entry on it is a live unguarded call, and nothing
   distinguishes a plain lookup grandfathered in from one that has since grown a
   rule. `store/testdb/` carries this caveat explicitly for its doubles
   (J: *"if one of them grows one these tests should fail rather than quietly
   keep passing"*); the worklist carries no equivalent.
3. **Name collisions in both directions.** Matching by name means an unrelated
   method called `Save` reads as a store write, and a store call made through a
   locally-defined interface with a renamed method reads as nothing at all. The
   `Rows` interfaces (`service/rows.go`, `infra/deploy`, `infra/volmove`) are
   exactly that shape — they are the sanctioned version, but the guard cannot
   tell a sanctioned one from a private escape hatch.
4. **Pure branching on domain state.** `if org.ConfigManaged()`,
   `if len(orgs) <= 1`, `if t.Kind == "cron"` import only `repo` types. No
   import guard and no call guard reaches them.
5. **Orchestration.** A handler calling two services and deciding between them
   is clean by every mechanical test and is §3.6 in its entirety.

A handlers→`infra/` import guard closes (1) and nothing else. It is worth
having for that reason and must not be presented as closing more.

### Two gates

**Gate A — verification, per wave.** Every row in §4 marked *agent-only* is a
**claim** (base rate: §4). **No agent-only row is implemented against until its
cited lines have been read.** The gate is **per wave**, not global: a row blocks
only the wave (or fix) that names it in "Where each bug gets fixed", and
reading those rows is the **first step of that wave**. Owner: me, before the
wave starts. The eight security/data-loss rows that used to hard-block
everything were read on 2026-09-21 and are **cleared** — results in §4.

**Gate B — O1 and O3.** ✅ **Clear** (2026-09-21). Neither is an open write
path (§1).

### Fix track — before and alongside the waves

*(Added in review.)* Live, verified bugs whose fix is small and local. Each is
its own commit, none waits for a wave, and none changes a service's shape.

| Row | The fix |
|---|---|
| **B34** | Right after wave 0. One `DeployService` helper — "redeploy on the image it runs if this is an upper env, else build" — used by the three redeploy paths. Wave 1 absorbs it. *Test:* a variable write on a running upper-env tile queues a deployment with `ImageTag` set. **Not blocked by Q5 (§6):** both answers include upper envs, so that half ships now; Q5 only decides whether the condition widens to every env. |
| **B14** | Burn conditionally (`AND used_at IS NULL`), check rows affected, burn **before** the join, stop discarding the error. Fail-closed on purpose: a join that fails after the burn leaves a spent invite and the owner re-invites — the reverse order is what lets two people in. *Test:* two concurrent redemptions of one open invite — exactly one joins. |
| **B15** | `createInvite` calls `MemberService.Invite`, like `addMember` beside it, and `validRole` (`v1/members.go:178`, its only caller) goes. `Invite` sends the mail itself (`service/member.go:101`), so the API's documented mail fix survives. **Behaviour change:** `Invite` refuses an empty email, so the API loses open (no-email) invites — the panel already lost them the same way (`org/members.go:310-321` goes through `Invite`). *Test:* a **route-level** test on the invites endpoint — unknown role, 10000-day expiry, empty email, each a 400. The existing `slugpath_test.go:160 TestUnknownRoleIsRefused` calls `MemberService.Invite` directly, so it never touched this route; that is how the bug survived beside it. |
| **B1** | `-y` skips the prompt and stops meaning `force`; mirror `infra.go:233-256`. |
| **B4** | `13-secret-declassification.md` as written. Q4 does not block it. |
| **B3** | Add `kind` to `UpdateBackup`'s `SET` list. |
| **B21, B22** | CLI sends only the halves it was given; stops blanking schedule fields and lets the server refuse. |
| **B35** | The error message only: drop "omit it to keep the stored value", which is wrong under `Replace` (the API's only verb) and deletes the secret if followed. The real fix is wave 3. |
| **B18** | Delete the stale sentence in `sharelink.go:40-44`. |

### Wave order

Each wave states what must be true to start (**enter**), what is true when it
is finished (**done**), and the one test that fails if the extraction is wrong
(**test**). A wave with no failing-first test is a rewrite, not an extraction.

At a glance — what each wave waits on:

```
0   boot panic (B0)           nothing                    ← start here
FT  fix track                 nothing (B34 right after 0)
1   deploy                    B34 landed
2   webhooks / prhook         1
3   variables & secrets       1, 2
4   work queue                nothing (parallel with 1–3)
5   storage & volumes         1, 4
6   authorization             nothing
7   settings & catalogue      Q3  ← darhvader
8   nodes                     0
9   store-level rules         1, 3
10  audit                     3, 6
11  ride-alongs a–f           nothing
```

Plus, on every wave: Gate A for its own agent-only rows, read first.

**Wave 0 — the boot panic (B0).** Not an extraction; a defect the sweep found.
*Enter:* nothing. *Done:* one `NodeService`, constructed once, assigned to
`rows.Nodes` **before** `nodeSvc.Sync` is called. *Test:* a boot test that
builds the wiring in `main.go`'s order and runs `Sync` against a swarm double
whose node state differs from the stored row — it panics today.
**Do this first regardless of everything else.**

**Wave 1 — Deploy.**
*Enter:* Gate A for B2 and B29 (B2 is verified; read B29 first). B34's fix
landed.
*Done:* `DeployService` exposes `Park`, `OnPush`/`Build`, `Rollback`,
`RecoverStale` and one `Log` read (O2); **ten** of the thirteen sub-service
call sites enter through it — `api/v1/lifecycle.go:75` (1) ·
`stackconf/apply.go:102,105,107,154` (4 — `stackconf` sits above service and
can hold it on `Ops`, §0) · `volmove.go:691` (1) · `cigate.go:103,112,123`
(3) · `cmd/stackrd/main.go:365` (1). prhook's three (`:436,441,720`) are
**wave 2's**; this wave only builds the doors they will use. The panel's
hand-rolled log read (`deployment/handler.go:179`) goes through `Log`.
`volmove.go:737`'s fifth copy of the terminal-state set is gone. B29's
redeploy pre-gate is deleted in favour of the service's.
`RecoverStale` **routes** the boot call through the service; lifting the
interrupted-run policy out of SQL is wave 9's.
*Test:* H's rule map (`sweep-h-deploy.md`) becomes a **table-driven test** —
one row per rule × per entry point, each cell holding the **expected** verdict,
not "refused" everywhere: cron is refused on `Trigger` and allowed on
`OnPush`/`Build` (§2 #4), managed goes to its own pipeline. It fails today on
every `ABSENT` and `disagree` cell, which is the point. Plus: an upper-env
rollback over the API is refused (B2).
*Explicitly not in scope:* the cron split (§2 #4) is two entry points, not one
rule — do not "fix" it by unifying.

**Wave 2 — Webhooks / prhook.**
*Enter:* wave 1 done; Gate A for B11 and B12 (B13 is verified).
*Done:* `prhook` holds a `DeployService` and no `*deploy.Engine` — its three
call sites (`:436,441,720`) enter through `Park`/`OnPush`; `watchMatch`
lives where the API deploy path, the CI gate and imagewatch can reach it;
`mergeVerdict` moves to the CI-gate owner unchanged (O5); PR close goes through
an `EnvironmentService` teardown that shares `teardown`'s cleanup but skips the
gate and running-check, as a PR close must (B13); B11, B12 closed.
*Test:* one push fixture per rule in `:424-434`, asserted against **both**
`Hook` and `HookConnector` (B12 is the two disagreeing); a CI-gated tile in a
PR env parks rather than deploys (B11).

**Wave 3 — Variables & secrets.**
*Enter:* waves 1–2 done (parked-tile release is a deploy concept); Gate A for
B5, B32, B37 (B7 and B35 are verified).
*Done:* the walk stays in `config/varref` (§3.2 — it cannot move up) and
becomes the **only** walk: `runner.go:773 checkRef` delegates to it and reads
the env rung (B5), `orgconf/storage.go:73`'s fourth resolver is deleted in its
favour. The handler sites in §3.2 stop importing `varref` and go through a
`VariableService.Lookup` door; the service gets a viewer-scoped `List` that
never loads a value the caller may not see (B37). One name grammar, one
bucket-mismatch message; `varref` returns a typed error that `refBroken` checks
instead of matching its wording (B32). The three `ClearWaiting*` functions
become one with a scope argument. The API gains a **merge** verb for variables
so the CLI stops its read-modify-write (B35). `syncBindings` revokes what it
granted — which needs the binding to record *who* created it (B7).
*Not here:* `syncBlob` stays until wave 9 deletes its cause.
*Test:* B5 as a failing test first — a file declaring `environments.<env>.vars`
plans clean and applies. B4's five tests from `13-secret-declassification.md`
stay as regression (the fix itself ships in the fix track). A CLI `vars set`
next to a masked secret succeeds and leaves the secret intact (B35). Then a
property test that the resolver and the plan gate agree on the same ref for all
four owner kinds.

**Wave 4 — Work queue.** The smallest wave, and it comes before storage because
storage needs the move-in-flight eligibility read it produces.
*Enter:* nothing. Can run in parallel with waves 1–3.
*Done:* `WorkItemService` has `Get`/`ListByStatus`/`Latest`; the two
eligibility reads are named methods ("is a move in flight for this tile",
"is this run still stoppable"); no `work_items` read outside the service.
*Test:* the read half of the deleted `readfree` guard, scoped to `work_items`
only — a mechanical assertion, and the one place a guard genuinely works.

**Wave 5 — Storage & volumes.**
*Enter:* waves 1 and 4 done (it needs wave 4's move-in-flight eligibility read,
and the mover's redeploy goes through wave 1's door); Gate A for B23, B26, B27.
*Done:* `VolumeService` exists; `StorageService` owns the probe *policy* and
the volume half of `DeletePath`; **one** answer to "does this tile hold data",
replacing the five (`volmove.go:243`, `placement.IsPinned:78`,
`placement.NodeOf:48`, `backup.VolumeFor:319`, `TileService.checkAttach`).
B23 (org share needs `export` on every backend), B26 (one tile-create
whitelist, volume→image forcing in the service), B27 (move targets from
resolved placement, never the raw column) closed.
*Test:* the five readings become one function with a table covering every
tile shape they currently disagree about — **write that table before touching
the code**, because H records two past data-loss incidents caused by exactly
those disagreements. Plus: `DropOrgShareVolumes`'s "abort the save if this
fails" contract becomes an assertion instead of a doc comment.

**Wave 6 — Authorization.**
*Enter:* nothing (O3 answered — not API-reachable); Gate A for B36.
*Done:* one gate parameterised by surface, not two copies
(`v1/auth.go:442-491`, `middleware/orgctx.go:387-476`); `ws.go`'s room map on
`AccessService`; the `ownedSettingsOrg`/`ownedOrg` aliases **deleted** (they are
false doc comments, not missing checks — do not add a check); the second role
vocabulary in `orgctx.go:151-285` collapsed onto `AccessService` levels.
"Can this account lose its powers" becomes one `AuthService` rule covering both
demote and disable (B16). The key grant ceiling moves into
`APIKeyService.Mint`, and key revoke gets an owner-scoped read (B36).
*Test:* the existing route tables (`routelevel_test.go`, `routeread_test.go`,
`routeverb_test.go`) run against **both** surfaces from one table. Today the
404-vs-403 divergence is the deliberate difference; it becomes a column, so the
next difference cannot arrive unannounced. Plus **one refusal test per
`KindDeferred` route** (§3.8) — five routes where the handler is the only
check. Write those first; they are what stops the move opening a hole.

**Wave 7 — Settings & catalogue.**
*Enter:* Q3 answered (`.templ` in scope or not). Nothing else waits on this
wave.
*Done:* one knob catalogue in `config/settings`, enumerated by all five
surfaces. *Test:* a test that the catalogue and each surface's rendered field
set agree per level — it fails today on four of the five.

**Wave 8 — Nodes.**
*Enter:* wave 0 done.
*Done:* the pre-join half (`ValidateAddress`, `AddNode`, `IssueKey`, `Claim`,
`Redeem`, `Sync`/`match`/`mirror`) decided by `NodeService`; one "add a node"
entry point, so `server/nodes.go:67` is one call; the boot gate at
`main.go:562-572` and the panel path apply the same rule.
*Test:* I's split table (`sweep-i-infra.md`, "Node lifecycle") as the
assertion — one test per stage, each naming its owner.

**Wave 9 — Store-level rules.**
*Enter:* waves 1 and 3 done — the receiving owners (`DeployService.RecoverStale`,
`VariableService`) must exist. *(Was "waves 1–8", which also chained it to Q3
through wave 7 for no reason: nothing here touches settings, nodes, auth or
storage.)*
*Done:* the four tile defaults (`sqlite/tiles.go:31-42`), `projectEnvVars`,
`SweepStaleRuns`' interrupted-run policy, the env-ladder ordering
(`stacks.go:302-306`) and the plan status sets (`stacks.go:264,221,229`) lifted
out of SQL. **`VariableService.syncBlob` is deleted in the same commit as
`projectEnvVars`** — it exists only to fight it (§3.2).
*Test:* for each, a round-trip — write through the service, read the struct
back, assert the field. B3's round-trip stays as regression (the one-line fix
ships in the fix track).

**Wave 10 — Audit.**
*Enter:* waves 3 and 6 done (they settle the actor vocabulary).
*Done:* `AuditService.Record` taking a **typed** actor, not a string;
`service/audit.go:11-14`'s doc comment — which currently argues *against* this
method — rewritten. Of the 13 back doors, **10** can call it directly —
`middleware/audit.go` (3), `api/v1/variables.go` (2), and — because they sit
above service (§0) — `envops` (1), `orgconf` (2), `stackconf/apply.go` (2). **Three cannot**:
`sharelink/sharelink.go:174,214` and `managedtiles/managedtiles.go:442` sit
below service, so they take an injected recorder interface — the `Links` /
`Rows` pattern those packages already use — carrying the same typed actor.
*Test:* the actor type is a closed set; a compile error is the assertion. Plus
a guard that `store/audit.Record` has no caller outside `service/` **and** the
recorder implementation — not "outside service/" alone, which the three
below-service writers cannot meet.

**Wave 11 — the ride-alongs.** *(Was "Not waves — ride along with whichever
wave touches them". No wave touches registry or orgs, so they had no home.)*
Each batch is a handful of collapses, its own commit, **enter: nothing** except
Gate A for its own rows. Any order, any time.

- **a — registry** (§3.12): B17, B19; the hand-built `<org-slug>_` prefixes
  onto `registry.Namespace`.
- **b — orgs, members, invites** (§3.13): B8; invite liveness onto one
  predicate; role folding removed on both surfaces; `OrgService.Rename` and
  `OrgService.Delete` take their rules.
- **c — managed instances and slices** (§3.10): B9, B25, B28, B38; the
  public-slice gate spelled once.
- **d — config plans** (§3.7): B20, B33; one `Waiting()` predicate on
  `repo.ConfigPlan`.
- **e — graph and canvas** (§3.9): the env-vs-stack `slugKey` drift (O4 — two
  meanings, pick one); plus the **notifier fan-out** — 22 sites outside
  `service/`, no design content, four services already doing it from inside
  their own writes. One mechanical pass.
- **f — app handler** (§4): B24, B30, B31.

---

## 6. Decisions still open for darhvader

Every open product call in this plan, in the order they block work. One at a
time. (Q1 and Q2 were answered in §5.)

| # | Decision | Blocks |
|---|---|---|
| **Q5** | B34 fix scope — restart on the running image in upper envs only, or everywhere? | Fix track B34 → wave 1 |
| **Q3** | Are `.templ` sources in scope for the sweep? | Wave 7; "final" status of every later wave |
| **Q4** | Can the panel still make a secret public, or only delete-and-recreate? | A follow-up only — B4 ships without it |
| **Q6** | B6 — wire `protected:` against the last applied file, or reject the key? | That one fix |

**Q5 — how far does the B34 fix go?** *(Review.)*
Today any variable, settings, volume or slice change rebuilds the tile from its
branch head (§3.1). The planned helper restarts on the running image **only in
upper envs** — which stops unpromoted code reaching prod and staging, but on
the bottom rung a variable change still ships whatever commits landed since the
last deploy. The other choice: **restart on the running image everywhere**, and
leave "build new code" to a push or a deploy button. Same helper either way;
only the condition changes.

**Q6 — the dead `protected:` flag (B6).** *(Review.)*
`StackRef.Protected` in an org config file is parsed and read nowhere, so
removing the stanza tears the stack down and deletes it (after the plan is
approved). The catch: the flag sits **inside the stanza being removed**, so it
can only protect anything by comparing against the **previously applied** file.
Either **wire it that way** (the org applier refuses to delete a stack whose
last applied stanza said `protected: true` — needs the previous file, not
small) or **reject the key at parse** (the file stops claiming a safety it does
not have — small). Wiring is a feature; rejecting is honesty.

**Q3 — are `.templ` sources in scope?** The sweep read `.go` only, so this is a
blind spot by construction, and two shards hit it:

- B: `app/panel.templ:389` (`liveDeployment` — "the image in use is the newest
  *done* build with a tag"), `:700` (`placementReason`), `app/app.templ:718`
  (`homeNodeName`) are category-1 domain rules, exercised by Go tests that live
  in the handler package.
- D: four of the five settings-knob lists are `.templ` files.
- *(Review.)* `app/panel.templ` **imports `config/varref`** — a template
  resolving variable references, not formatting them.

`*_templ.go` is generated and off-limits per CLAUDE.md; the `.templ` **sources**
are not. A grep over `.go` will never find these.

This is bigger than a wave-7 gate. *(Review.)* It is really **"is the sweep
complete?"** — if the answer is yes, `.templ` sources need the same read the
`.go` files got, and that read could add rows or reorder waves. Only wave 7
waits on it directly (wave 9 no longer does), but no wave after the fix track
should be called final until it is answered.

**Q4 — the declassification panel path** (from `13-secret-declassification.md`):
refusing means the panel can no longer declassify at all, only delete-and-recreate.
Still unanswered; the fix is the same either way, so **B4 ships in the fix track
without waiting for it**.

---

## 7. Corrections applied to other files in this directory

All three are done. Each edited file now points back at §2 rather than silently
disagreeing with it.

- `11-business-logic-sweep.md` — the Backups bullet carried B's framing, which
  G and J override. Rewritten; the API delete was named as the unguarded
  path. §2 #1. *(Superseded by the review — see below.)*
- `shards/sweep-e-boot.md` — `seed.go` skips one rule, not four. Corrected,
  severity high → medium. §2 #2.
- `shards/sweep-h-deploy.md` — `ResolveInfraPath` HIGH → debt. §2 #3.

From the 2026-09-21 review:

- `11-business-logic-sweep.md` — the Backups bullet re-corrected: the API
  delete reloads the scheduler at `:207`, so neither path is a live bug. §2 #1.
- `shards/sweep-g-config.md` — the header's layering premise (§2 #5), the
  backup-delete consequence and its divergence-table row (§2 #1), the
  sharelink entry (B18).
- `shards/sweep-i-infra.md` — the panel-backup "bypass is live" (§2 #1).
- `shards/sweep-b-panel.md` — the last-admin entry (O3 answered) and the
  setup-domain entry (B8 downgraded).
- `shards/sweep-f-cli.md` — the coupled-pair entry (B21: inherit, not zero).

A shard is evidence, not scripture. When this file and a shard disagree, this
file is the resolved verdict and the shard should be edited to say so — not
left to be re-read cold by whoever picks the work up.
