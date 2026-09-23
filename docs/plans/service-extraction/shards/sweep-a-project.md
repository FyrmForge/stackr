# Sweep A — project/annotate/deployment/container/db handlers

Files read: (every non-generated `.go` in the five packages; 24 files, 6431 lines)

| File | Lines |
|------|-------|
| `internal/stackrd/handlers/web/handler/project/handler.go` | 2467 |
| `internal/stackrd/handlers/web/handler/project/stackgraph.go` | 526 |
| `internal/stackrd/handlers/web/handler/project/commitlog.go` | 503 |
| `internal/stackrd/handlers/web/handler/project/releases.go` | 334 |
| `internal/stackrd/handlers/web/handler/project/envcompare.go` | 145 |
| `internal/stackrd/handlers/web/handler/project/staging.go` | 107 |
| `internal/stackrd/handlers/web/handler/project/placement.go` | 91 |
| `internal/stackrd/handlers/web/handler/project/releases_test.go` | 111 |
| `internal/stackrd/handlers/web/handler/project/stackgraph_test.go` | 104 |
| `internal/stackrd/handlers/web/handler/project/guard_test.go` | 97 |
| `internal/stackrd/handlers/web/handler/project/commitlog_test.go` | 64 |
| `internal/stackrd/handlers/web/handler/project/unset_test.go` | 60 |
| `internal/stackrd/handlers/web/handler/project/refs_test.go` | 55 |
| `internal/stackrd/handlers/web/handler/project/testhandler_test.go` | 36 |
| `internal/stackrd/handlers/web/handler/annotate/annotate.go` | 66 |
| `internal/stackrd/handlers/web/handler/annotate/groups.go` | 53 |
| `internal/stackrd/handlers/web/handler/deployment/handler.go` | 208 |
| `internal/stackrd/handlers/web/handler/deployment/stream_test.go` | 21 |
| `internal/stackrd/handlers/web/handler/container/handler.go` | 267 |
| `internal/stackrd/handlers/web/handler/container/terminal.go` | 54 |
| `internal/stackrd/handlers/web/handler/container/list_test.go` | 60 |
| `internal/stackrd/handlers/web/handler/db/handler.go` | 562 |
| `internal/stackrd/handlers/web/handler/db/data.go` | 262 |
| `internal/stackrd/handlers/web/handler/db/files.go` | 178 |

## Findings

### project/handler.go:1561 vs db/handler.go:468 — an unparsable numeric field
- **Rule:** what a typo in a port / limit box means. `atoiOr` (`project/handler.go:1561-1567`, used at `:481` for `container_port` on tile create) folds anything unparsable, negative or >65535 to the caller's default, i.e. "not set". `formInt` (`db/handler.go:468-478`, used at `:452-454` for `external_port`, `mem_limit_mb`, `shm_size_mb`) returns `-1` **on purpose** so the service refuses, and its comment names the bug it fixes: "the panel used to fold a typo into 'not set', which silently unpublished a port or dropped a memory cap."
- **Category:** 2 (validation of domain shape), 6 (two surfaces, drifted).
- **Owner it should have:** the tile / instance service — one parse-or-refuse helper both create and update paths call.
- **Duplicated at:** `db/handler.go:481-491` (`formFloat`, same `-1` contract for `cpu_limit`).
- **Severity:** **bug** — the fix landed on the db side and not on tile create; the two spellings of the same question already disagree, with a comment on one side saying the other spelling was wrong.

### project/handler.go:986 — node-id key translation before SavePositions
- **Rule:** an env-canvas position is stored under `app:<slug>`; a stack-canvas position under the raw DOM id.
- **Category:** 6 (same service method, two node-key meanings).
- **Owner it should have:** `GraphService.SavePositions` (take the tiles, or take a typed key).
- **Duplicated at:** `project/stackgraph.go:88` builds `repo.NodePosition` with no `slugKey`, saved at `:93`, in a function whose own comment at `:63` claims it "mirrors" the env one. Read side is symmetric-but-different: `handler.go:1183` re-maps with `uuidKey`, `stackgraph.go:259` does not. There is no api/v1 positions route, so this is intra-web drift only.
- **Severity:** **bug** — already established in `11-business-logic-sweep.md`; confirmed at the cited lines.

### db/handler.go:272-276 — deploy detached on `context.Background()`
- **Rule:** an instance deploy outlives the request, and its failure is a log line only.
- **Category:** 3 (orchestration), 5 (side effect fired by the caller).
- **Owner it should have:** the work queue, the way `PlanService.Approve` enqueues (`project/handler.go:672`, see its comment at `:667-671` — the identical problem, solved there).
- **Duplicated at:** - .
- **Severity:** **bug** — a failed deploy reaches nobody; the user is told "Database deploying" (`:277`) regardless, and there is no row to poll.

### project/commitlog.go:444-490 — `envDeployments`, the deployment state machine
- **Rule:** what an environment "runs", what is "in flight", what "failed", what commits have artifacts — read out of `repo.Deployment` rows with `deploystate.Done` / `IsLive` / `Error` branches, a "only the tile's newest attempt counts as failed" rule (`:479`) and a per-tile 5-row window (`:451`).
- **Category:** 1, 3.
- **Owner it should have:** `DeployService` (or a new `ReleaseState` read model); `h.deploys.ForTile` is the only service call in it.
- **Duplicated at:** `internal/stackrd/handlers/api/v1/releases.go:56-93` — the same aggregation written again (git-only skip at `:61`, `ForTile(..., releaseWindow)` at `:64` with its own `releaseWindow = 5` at `:30`, newest-done = runs at `:88-92`). **Already drifted:** web tracks live/failed/`envProgress` and counts only `j == 0` as failed; the API collapses every non-done row into one `building` bool via `deploystate.IsLive` (`:87`). Web iterates every env, the API filters `Type != "static"` (`:51`).
- **Severity:** **bug** — two surfaces answer "what does this env run" differently, and the API's answer feeds the CLI (`internal/cli/cmd/promote.go:33`, `internal/cli/lifecycle.go:329`).

### project/commitlog.go:285-429 — the chip placement rules
- **Rule:** one chip per env per commit; a "building" chip is stack-level and colourless (`:368`); a second env building the same commit keeps one chip (`:372`); a queued deploy with no commit is the head (`:353`); ephemeral envs are skipped (`:335`); the default env owns the stack-scoped plan row (`EnvSlug ""`, `:385`); a clean plan row is dropped (`:396`).
- **Category:** 1.
- **Owner it should have:** needs a new release/commit-log read service; the page should render a struct, not derive it.
- **Duplicated at:** - .
- **Severity:** debt.

### project/commitlog.go:140-144 — package-global mutable cache
- **Rule:** one commit list per stack, refreshed at most once a minute, plus an `older` map guarded by a package `sync.Mutex`; stale-on-error fallback at `:163-171`.
- **Category:** 5 (cache policy owned by a handler).
- **Owner it should have:** whichever service owns the commit log; a package-level `sync.Map` in a handler is process-global state no service can invalidate.
- **Duplicated at:** `project/commitlog_test.go:44` and `releases_test.go:95` write `logCache` directly — the tests can only reach the rule by poking the handler's globals.
- **Severity:** debt.

### project/commitlog.go:247-281 — where commits come from
- **Rule:** a config-managed stack reads GitHub through its connector; otherwise fall back to the local clone of the first `SourceType == "git"` tile of the first non-ephemeral env, `origin/<branch>` then bare HEAD.
- **Category:** 1, 4 (`h.gh`, `gitlog`, `h.engine().RepoDir(t)`).
- **Owner it should have:** needs a new commit-log service; `h.engine()` (`handler.go:577`) is a raw `*deploy.Engine` reach-through.
- **Duplicated at:** - .
- **Severity:** debt.

### project/releases.go:89-105 — `planWaiting`
- **Rule:** a pending config plan blocks/warns a promote when its commit is at or before the one promoted; a stack-scoped row (`EnvSlug ""`) covers every env; unknown distances count as waiting.
- **Category:** 1.
- **Owner it should have:** `ReleaseService` (it already owns `Target` and `Promote`, `releases.go:174`, `:306`; service at `internal/stackrd/service/release.go:49`, `:90`).
- **Duplicated at:** `internal/stackrd/handlers/api/v1/releases.go:105-110` — the API's pending-plan match is a plain `CommitSHA == sha` equality. It honours neither the stack-scoped row (`EnvSlug == ""`) nor the "at or before the commit" rule this copy has at `releases.go:92-101`.
- **Severity:** **bug** — the API says "no plan waiting" in two cases where the panel warns; same promote, two answers.

### project/releases.go:111-114 — "Envs[0] is the rung that builds on push"
- **Rule:** the ladder's bottom rung is never a promote target.
- **Category:** 1.
- **Owner it should have:** `ReleaseService`. Note the shape rule is spelled a second way at `:258-277` (`skippedRungFor` re-derives the ladder from `Type == "static"` order) and a third time inside the service, which owns `Target`. Three spellings of "what is a rung".
- **Duplicated at:** `project/releases.go:258-277`; `project/commitlog.go:334-336` (ephemeral skip).
- **Severity:** debt.

### project/releases.go:223-254 — `promoteSpan`
- **Rule:** what a promote carries, and when it is a rollback, from window positions plus connector-reported distances.
- **Category:** 1.
- **Owner it should have:** `ReleaseService`.
- **Duplicated at:** - .
- **Severity:** debt (has a test, `releases_test.go:29`).

### project/envcompare.go:33 and :128 — planner constructed by hand
- **Rule:** how to get a config snapshot. `stackconf.Planner{Store: h.store}` is built literally, twice, while the handler already holds the wired planner (`handler.go:572`, `h.applier.Planner`).
- **Category:** 3, 4.
- **Owner it should have:** the plan/config service; at minimum `h.planner()`.
- **Duplicated at:** `project/envcompare.go:128` (second site, inside `CopyEnv`). Repo-wide these are the only two non-test literal constructions besides the real one at `cmd/stackrd/main.go:627`, and **both omit `Src`** (main.go passes `{Store: store, Src: gh}`; type at `config/stackconf/runner.go:40`), so `Snapshot` here runs with no git source.
- **Severity:** debt — bordering on bug: the snapshot the compare widget and `CopyEnv` work from is produced by a differently-configured planner than every other snapshot in the product.

### project/envcompare.go:122-144 — `CopyEnv` orchestration
- **Rule:** refuse on a config-managed stack; refuse a missing cell; take the reference env's tile config but keep this env's domains ("hostnames are the one thing that is per environment by construction", `:138`); apply; then reload the scheduler.
- **Category:** 1, 3, 5 (`h.sched.Reload(ctx)` at `:143` fired by the caller, not by `applier.CopyTile`).
- **Owner it should have:** needs an env-compare/copy service, or `StackService`.
- **Duplicated at:** the config-managed refusal is spelled at `staging.go:59`, `handler.go:729`, `db/handler.go:85`.
- **Severity:** debt.

### project/handler.go:1043-1096 — `EnvLogsStream` tile selection + runtime calls
- **Rule:** which tiles have a followable log: volumes never (`:1049`), anything whose `runpolicy` is not `KeepAlive` never (`:1052`), prefer the swarm *service* stream and fall back to a container by label, choosing `LabelDB` for a managed tile (`:1070-1073`).
- **Category:** 1, 4 (`h.rt`, `h.clus`, `envnet.ServiceFor`).
- **Owner it should have:** a log/stream service; `db/handler.go:207-252` is the same fallback written again.
- **Duplicated at:** `db/handler.go:224-235` (service-name-then-label fallback, `LabelDB`, `h.clus.Self`), `container/handler.go:244`.
- **Severity:** debt.

### project/handler.go:391-418 — `ResetEnvironment`
- **Rule:** tear the env down, keep DB volumes, then re-plan **synchronously** because the service's background replan would leave a stale diff on screen (`:408-411`).
- **Category:** 3, 5.
- **Owner it should have:** `EnvironmentService.Reset` should return the new plan (or take a "plan synchronously" option).
- **Duplicated at:** `handler.go:791-825` (`SetPlanInput`: set, then `h.planner().RunAll` for the same reason, comment at `:807`), `handler.go:742-747` (`SaveEnvConfig`), `handler.go:611-619` (`SaveConfigBinding` → `h.runPlan`).
- **Severity:** debt — four handlers each decide when a re-plan happens and whether it blocks.

### project/handler.go:445-517 — `CreateTile` field coercion
- **Rule:** `source_type` ∈ {git,image} else "git"; `kind` ∈ {cron,function,volume} else "service"; a volume forces `source_type = "image"` (`:463`); `run_on_deploy` only means anything for a function (`:478`); a create on an unmanaged stack builds the staged config patch here, including resolving the attach target's slug (`:492-501`).
- **Category:** 1, 2.
- **Owner it should have:** `TileService.Create` — it already takes the row and the patch; the vocabulary of kinds is the domain's, not the form's. The real fallback already lives at `internal/stackrd/service/tile.go:362-368` (empty SourceType → "image" if ImageRef set else "git") with validation at `service/tilevalidate.go:79-121`.
- **Duplicated at:** `internal/stackrd/handlers/api/v1/apps.go:67-73` — kind defaults to "service" but is validated against `runpolicy.For`, a **different** whitelist from this one, and there is **no `source_type` coercion at all** (`in.SourceType` passed raw at `:76`). Volumes go through a separate endpoint, `api/v1/volumes.go:50-55`, which hard-codes `Kind: "volume"` and never sets `SourceType`, while `service/tilevalidate.go:117` asserts a volume's `SourceType != "image"`. CLI: `internal/cli/cmd/tile.go:120-127`.
- **Severity:** **bug** — the volume→image forcing exists only in this web handler; an API-created volume relies on `tile.go:362`'s empty-ImageRef branch to land somewhere legal. Three surfaces, three whitelists.

### project/handler.go:1525-1557 — `CreateDB` scope coercion
- **Rule:** the panel may only pick `env` or `stack` scope; anything else becomes `env`; org scope is CLI/API-only (`:1537-1542`). Documented reason in the comment, and still a domain rule in a handler.
- **Category:** 1.
- **Owner it should have:** `ManagedInstanceService.Create` (with a "surface" or allowed-scopes argument).
- **Duplicated at:** `db/handler.go:85-90` and `db/handler.go:158-161` both re-decide what org scope means (below).
- **Severity:** debt.

### project/handler.go:2142-2214 — `MintStackLink` validation
- **Rule:** name must match `^[A-Za-z0-9_][A-Za-z0-9_.-]*$` (`:2134`); at least one field; TTL 1–720 hours; reveal window 0–60 minutes; kind defaults to drop unless exactly `share`.
- **Category:** 2.
- **Owner it should have:** `sharelink` / `RevokeService` — the name rule already lives in the variable service, and the comment at `:2130-2133` admits this is a second spelling kept for the form.
- **Duplicated at:** `handler.go:798` (same `varNameOK` for plan inputs); org-level mint pending shard B.
- **Severity:** debt.

### project/handler.go:1814-1852 — `unsetSecrets`
- **Rule:** a stack-declared secret counts as set if the stack or org has it, **or** if every environment has it; an env missing it means unset.
- **Category:** 1.
- **Owner it should have:** `VariableService` / the plan service — this is the resolution order `varref` already owns, restated.
- **Duplicated at:** `varref` resolution order (config/varref), not re-read in this shard.
- **Severity:** debt (has tests, `unset_test.go:14`, `:35`).

### project/handler.go:2263-2302 — stack domain-resource writes
- **Rule:** `DeleteStackDomain` re-checks `r.Level != "stack" || r.OwnerID != p.ID` (`:2294`) because the id is a form field.
- **Category:** 1 (tenancy/ownership decision in a handler).
- **Owner it should have:** `DomainResourceService.Delete` should take the owner and refuse there.
- **Duplicated at:** `handler.go:2223-2238` (`RevokeStackLink` does the same "is this id mine" loop over `h.revoke.Links`), `db/data.go:250-260`, `db/files.go:123-128`, `db/handler.go:403` (`slices.Find`, the one that is already in a service).
- **Severity:** debt — five hand-rolled scope checks; `slices.Find` shows the intended shape.

### project/handler.go:1401-1425 / :1427-1495 / stackgraph.go:247-418 — canvas assembly
- **Rule:** which tiles draw edges (volumes and managed tiles do not, `:1405`), dedupe rules, "a slice card already carries this dependency" (`:1483`), and in `buildStackGraph` the whole level-of-the-tree rule for managed instances (`stackgraph.go:310-357`: stack-scoped = resident card, org-scoped = ghost, env-scoped = inside the env card) plus the traffic-endpoint mapping.
- **Category:** 1, 3.
- **Owner it should have:** needs a new canvas/graph read service (`GraphService` today only stores positions, annotations and groups).
- **Duplicated at:** the org canvas (shard C) renders the same scope tree one level up; `handler.go:1427` `sharedRefs` is called from both `buildGraph:1212` and `buildStackGraph:363` with different post-filters.
- **Severity:** debt.

### project/placement.go:23-82 — `markPlacement`
- **Rule:** chips only when there are two nodes or a replicated tile (`:34`); a volume has no service (`:44`); "no tasks" ≠ "0 running" (`:58-60`); move percentage (`:74`).
- **Category:** 1, 4 (`h.rt.ListNodes`, `h.rt.ServiceTasksOnNetwork`, `envnet.Resolve`, `h.mover`).
- **Owner it should have:** `NodeService` / a placement service; `placement.InGroup(ctx, h.store, h.rt, ...)` is also called straight from `handler.go:860`.
- **Duplicated at:** `project/handler.go:857-867` (`PlanView` re-checks moves against live placement and renames nodes via `h.nodeSvc.ByNodeID`).
- **Severity:** debt.

### project/handler.go:2382-2394 — `ops()` builds a `managedtiles.Service` on the fly
- **Rule:** which deps env teardown needs. A second `managedtiles.NewService(...)` is constructed per call, beside the one the db handler is injected with (`db/handler.go:29`).
- **Category:** 3, 4.
- **Owner it should have:** DI in `server.go`; and `envops` itself looks like the env service's body living outside it.
- **Duplicated at:** `db/handler.go:29`/`:47` (injected instance of the same service).
- **Severity:** debt.

### project/handler.go:924-931 — `resyncProxy`
- **Rule:** re-render every tile's proxy config after a settings save. Dead as written: no caller remains in this package (`SaveSettings:911` and `SaveEnvSettings:2320` go through `h.settings` and do not call it), so either the side effect moved into `SettingsService` and this is a leftover, or it moved and nothing fires it.
- **Category:** 5.
- **Owner it should have:** `SettingsService`.
- **Duplicated at:** - .
- **Severity:** debt (verify it is dead before deleting; if `SettingsService` does **not** resync, this is a bug).

### project/staging.go:59-62 — apply-time managed re-check
- **Rule:** staged rows can outlive the UI-managed period, so ownership is re-checked at apply, not only at stage. Documented reason at `:56-58`.
- **Category:** 1.
- **Owner it should have:** `applier.ApplyStaged` / `TileService`, since the same force-apply is reachable from elsewhere.
- **Duplicated at:** `project/handler.go:492` (stage-time branch), `envcompare.go:122`, `db/handler.go:85`.
- **Severity:** debt — listed with its documented rationale; the rule is right, the location is not.

### project/handler.go:366-377 and :401-406 — "typing the slug is this surface's force"
- **Rule:** the web confirm dialog substitutes for the running-resources check, so `Delete`/`Reset` are called with `force = true`; the API and CLI take a flag instead.
- **Category:** 1.
- **Owner it should have:** fine as a surface decision, but it means the web can never hit the service's safety check. Worth a row because the service's guard is unreachable from the panel by construction.
- **Duplicated at:** `db/handler.go:426-432` takes the **opposite** choice (confirm **and** `force = false`, comment at `:429-431`). Two confirm-then-destroy surfaces, two meanings of the confirmation.
- **Severity:** debt.

### project/stackgraph.go:96,110,122,136,148,162,174,188,200,224 + handler.go:994,1008 — notifier fan-out
- **Rule:** every canvas mutation re-broadcasts to other open canvases.
- **Category:** 5.
- **Owner it should have:** `GraphService` — 12 call sites in this shard, every one of them the line after a `h.graph.*` write. Four services already do their own notifying (`service/variable.go:280`, `service/slice.go:336`, `service/tilelifecycle.go:272`, `service/managedinstance.go:418`), so the pattern exists; the graph writes just don't use it.
- **Duplicated at:** as listed (the sweep doc cites 11; `stackgraph.go:96` makes 12). Repo-wide 22 sites sit outside `service/`; the rest are `prhook/handler.go:339,355`, `infra/jobs/jobs.go:391,406,412,420,460`, `infra/deploy/engine.go:559`, `infra/metrics/reconcile.go:112`.
- **Severity:** debt.

### project/stackgraph.go:206-212 — `loadEnvForAnnotation`
- **Rule:** the doc comment says it "resolves + authorizes the env in the URL"; the body only resolves. Authorization is the route gate's (`guard_test.go:83-97` confirms the gate moved).
- **Category:** — (stale comment, not a rule).
- **Owner it should have:** n/a; fix the comment so nobody re-adds a check here or removes the gate believing this one exists.
- **Duplicated at:** - .
- **Severity:** trivial.

### annotate/annotate.go:35-37, groups.go:24-26 — server mints the id
- **Rule:** a save with no id is a create and gets a fresh uuid; validation is delegated to `repo.ValidateAnnotation` / `repo.ValidateGraphGroup`.
- **Category:** 2 (thin — the rule itself lives in `repo`).
- **Owner it should have:** `GraphService.SaveAnnotation` could mint the id; as written the package is a deliberate shared HTTP half (package doc, `annotate.go:1-4`).
- **Duplicated at:** - .
- **Severity:** trivial.

### deployment/handler.go:84-88 — "image tag required"
- **Rule:** a rollback needs a tag; the handler refuses before the service is asked.
- **Category:** 2, 6.
- **Owner it should have:** `DeployService.Rollback` — which this handler does use (`:88`). The rule itself is at `service/deploy.go:55-57`.
- **Duplicated at:** `internal/stackrd/handlers/api/v1/lifecycle.go:72-74` — its own inline 400 "image_tag required", and then at `:75` it calls `a.engine.EnqueueRollback` **directly**, skipping `DeployService.Rollback` entirely and with it the whole `deployable()` rule set (`service/deploy.go:63-87`: volume / managed / cron / **upper-env promote-only** refusals). Route: `api/v1/v1.go:429`.
- **Severity:** **bug** (in shard D's file, surfaced from here) — an API key holder can roll back a tile in an upper environment, which the promotion ladder is meant to forbid. The web copy is the correct one.

### deployment/handler.go:118-124 — `Cancel` drops the error
- **Rule:** cancel, then redirect regardless. `h.deploys.Cancel(...)` returns nothing to the user; a cancel that did not take looks identical to one that did.
- **Category:** 3.
- **Owner it should have:** `DeployService.Cancel` (should return an error the page surfaces).
- **Duplicated at:** - .
- **Severity:** debt.

### deployment/handler.go:179 — `h.engine.LogPath(d.ID)` read straight off disk
- **Rule:** where a build log lives, and that the stream replays it before following.
- **Category:** 4.
- **Owner it should have:** `DeployService` (the handler holds `*deploy.Engine` purely for this).
- **Duplicated at:** the API serves the same file raw (comment at `:158-160`); confirm the API's own `LogPath` read in shard D.
- **Severity:** debt.

### container/handler.go:100-117 — swarm-or-not branch
- **Rule:** fewer than two nodes (or a failed node list) means "the local socket is the whole answer"; otherwise render section shells and let each node fetch itself.
- **Category:** 1, 4 (`h.clus` throughout the file).
- **Owner it should have:** `ContainerService` — it already owns the system-container guard (`:207-218`; guard at `service/container.go:31`, `:62`), so the listing rules are the odd ones out.
- **Duplicated at:** - . There is no api/v1 containers surface, and this handler is `ContainerService`'s only consumer (wired once at `cmd/stackrd/main.go:853`).
- **Severity:** debt (single caller, misplaced only).

### container/handler.go:80-94 — `applyFilter`
- **Rule:** hide non-running rows unless asked, but always count them, because an exited agent/proxy row is the only place its remove button lives.
- **Category:** 1.
- **Owner it should have:** arguably display policy; listed because "which containers are corpses" is a domain statement and it is tested (`list_test.go:19`).
- **Duplicated at:** - .
- **Severity:** trivial.

### container/handler.go:175-196 — `inspect` failure taxonomy
- **Rule:** "No such container" (string match on a flattened agent error) = 404; deadline/net error = render the page with `down = true` because the container is probably still there; anything else = 500.
- **Category:** 1, 4.
- **Owner it should have:** `ContainerService` / `cluster` should return typed errors; a string match on `"No such container"` is one docker message change away from turning a 404 into a 500.
- **Duplicated at:** - .
- **Severity:** debt.

### container/terminal.go:27 and :38 — exec guard spelled twice
- **Rule:** `EnsureExecAllowed` before the page and again before the socket. Documented reason at `:25-26` ("so the answer is a sentence rather than a terminal that opens and then fails its handshake") — deliberate, and correct.
- **Category:** 1.
- **Owner it should have:** already `ContainerService`. Listed only as the good example the rest of the shard should look like.
- **Duplicated at:** `container/terminal.go:38`.
- **Severity:** trivial (intentional).

### db/handler.go:85-90 and :158-161 — "an org-scoped instance is never file-owned"
- **Rule:** the config file cannot declare an org-scoped instance, so the managed refusal is skipped for it. Written once as a refusal (`requireFileUnowned`) and once as a display flag (`loadTab`, `td.fileOwned`).
- **Category:** 1, 6 (two spellings, one file).
- **Owner it should have:** the stack/instance service — one `FileOwns(tile) bool` both call.
- **Duplicated at:** `db/handler.go:158-161`.
- **Severity:** debt — they agree today; the display copy silently omits the `ScopeKind == "org"` early return's sibling conditions, so a change to one will not reach the other.

### db/handler.go:323-344 — provision grouping
- **Rule:** consumer rows collapse into one entry per logical database; `Public` is per-bucket so the first row wins; `Status == "orphaned"` marks the group.
- **Category:** 1.
- **Owner it should have:** `SliceService`.
- **Duplicated at:** - .
- **Severity:** debt.

### db/handler.go:360 — engine-specific noun built in the handler
- **Rule:** the flash says "database" or "bucket" depending on `d.Engine` (`unitNoun`), title-cased by slicing the first byte.
- **Category:** 2 (weak) — string policy, but it is engine vocabulary.
- **Owner it should have:** `managedtiles.Engines` already holds per-engine capability flags (`DataBrowser`, `FileBrowser`); the noun belongs beside them.
- **Duplicated at:** `db/handler.go:380` (fork failure), `:413`/`:415` (hard-coded "Bucket" instead of `unitNoun`).
- **Severity:** trivial.

### db/data.go:58-96 — credential resolution for the data browser
- **Rule:** a db name that is neither the instance's own nor a provisioned one 404s; the instance's own name yields superuser creds, a slice yields that slice's user; `locked` with no db is a 404. Comment at `:20-26` states plainly that `locked` is display-only and containment rests on postgres grants.
- **Category:** 1, 4 (`managedtiles.PGCreds` assembled by hand from tile columns).
- **Owner it should have:** `ManagedInstanceService` / `SliceService` — credentials should never be assembled in a handler.
- **Duplicated at:** `db/data.go:246-260` (`PGDBPanel` re-walks `ForInstance` to find the same slice), `db/files.go:110-129` (`loadBucket`, same walk for buckets). No surface outside this file calls the `PG*` browse methods (`infra/managedtiles/sqlbrowse.go:101,123,134,142`), so this is misplacement, not drift.
- **Severity:** debt (single caller).

### db/data.go:110 vs :172-225 — write check computed but not enforced here
- **Rule:** `v.canWrite = stackrmw.CanWriteHere(c)` is used for rendering; `DataUpdate`, `DataDelete` and `DataInsert` run `PGUpdateCell` / `PGDeleteRow` / `PGInsertRow` with no write check of their own.
- **Category:** 1.
- **Owner it should have:** the route gate (that is where the other write gates moved, `guard_test.go:51-53`).
- **Duplicated at:** - .
- **Severity:** **unconfirmed** — not called a bug: the route gating for `/dbs/:id/data/*` is in `server.go` (shard C). If the gate is there, this is trivial; if it is not, the read-only view's Edit buttons are hidden but the endpoints are open. **Confirm in shard C.**

### db/data.go:202-225 — insert column selection
- **Rule:** form fields prefixed `v_` become columns; an empty value is omitted so the column default applies.
- **Category:** 2.
- **Owner it should have:** `managedtiles` (it owns `PGInsertRow`); the prefix convention is the form's, the "empty means default" rule is the database's.
- **Duplicated at:** - .
- **Severity:** trivial.

### db/files.go:21-36 — volume identity for file routes
- **Rule:** only a managed tile has a data volume; the node comes from `h.clus.NodeOf(...)` rather than `d.HomeNode` so a home-less instance gets a 409 instead of browsing the wrong disk; the volume name is `managedtiles.VolumeName(d)`.
- **Category:** 1, 4.
- **Owner it should have:** `managedtiles.Service` — it already owns `DataVolume` (one caller, `db/handler.go:131`).
- **Duplicated at:** `db/handler.go:126-130` (`VolumePanel` repeats the `IsManaged` gate); `internal/stackrd/handlers/web/handler/app/files.go:26` + `:30` — the same two lines for the app side, differing only in `a.DockerVolume()` vs `managedtiles.VolumeName(d)`, which resolve to the same value (`store/repo/models.go:732` vs `infra/managedtiles/managedtiles.go:371`).
- **Severity:** debt.

## Clean files

- `internal/stackrd/handlers/web/handler/project/testhandler_test.go` — test wiring only.
- `internal/stackrd/handlers/web/handler/project/guard_test.go` — asserts the route gate, no rules of its own.
- `internal/stackrd/handlers/web/handler/project/refs_test.go`, `stackgraph_test.go`, `releases_test.go`, `unset_test.go`, `commitlog_test.go` — tests; they *assert* rules listed above rather than owning them. (`commitlog_test.go:44` and `releases_test.go:95` write the package-global `logCache`, noted above.)
- `internal/stackrd/handlers/web/handler/deployment/stream_test.go` — SSE framing only.
- `internal/stackrd/handlers/web/handler/container/list_test.go` — filter only.
- `internal/stackrd/handlers/web/handler/annotate/groups.go` — same thin shape as `annotate.go`, nothing beyond id minting.

## Cross-surface checks still open

- `db/data.go:110` write gate — needs the `/dbs/:id/data/*` route gating in `server.go` (shard C) to settle bug vs trivial.
- `deployment/handler.go:179` build-log path — confirm whether api/v1 reads `LogPath` itself (shard D).

## Handed to other shards

Found from this shard, belongs in shard D's file:

- `internal/stackrd/handlers/api/v1/lifecycle.go:75` calls `a.engine.EnqueueRollback` directly, bypassing `DeployService.Rollback` and the whole `deployable()` rule set (`service/deploy.go:63-87`). Upper-environment rollback is reachable with an API key; the promotion ladder forbids it everywhere else. Route `api/v1/v1.go:429`.
- `internal/stackrd/handlers/api/v1/releases.go:35-115` re-implements `commitlog.go`'s deployment aggregation and pending-plan match, both weaker than the web copy.
- `internal/stackrd/handlers/api/v1/apps.go:67-73` + `volumes.go:50-55` — the tile-create coercion the web handler does, minus `source_type`, against a different kind whitelist.

Shared-with shard C: `handlers/web/handler/app/files.go:26-30` is line-for-line `db/files.go:31-35`.
