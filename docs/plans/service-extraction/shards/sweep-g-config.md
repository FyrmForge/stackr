# Sweep G — config packages

Scope: every non-generated, non-test `.go` file under
`internal/stackrd/config/{envcompare,envops,envutil,orgconf,runpolicy,secrets,
settings,sharelink,stackconf,staging,varref}`. 32 files, 12 270 lines,
every one read top to bottom.

Context that shapes every verdict below: `config/` sits **below** `service/`
(service imports config, not the reverse — see `stackconf.Applier.Ops` holding
`*service.TileService`, `*service.VariableService`, … and `sharelink.Links` /
`staging.Staged` being interfaces precisely because "that package is built on
this one"). So "it reads the store" is not the test. The test is: **is the rule
this code enforces a rule a service should own, and does any other surface
spell it differently?**

> *Corrected 2026-09-21 (`12-business-logic-plan.md` §0, §2 #5):* half right.
> `stackconf`, `orgconf` and `envops` **import** `service` — `Ops` holding
> `*service.TileService` is the proof — so they sit *above* it and are callers,
> graded like handlers. Only `varref`, `settings`, `secrets`, `staging`,
> `envutil`, `runpolicy`, `sharelink` and `envcompare` sit below. The
> misplaced-owner verdicts below rest on caller count and mostly stand.

Files read (line counts as `wc -l`):

```
  239 envcompare/envcompare.go
  466 envops/envops.go
  105 envutil/envutil.go
 1088 orgconf/orgconf.go
  251 orgconf/storage.go
  111 orgconf/moved.go
   99 orgconf/job.go
  161 orgconf/export.go
   42 runpolicy/runpolicy.go
  158 secrets/secrets.go
   54 settings/keys.go
  445 settings/settings.go
  225 sharelink/sharelink.go
  204 stackconf/slices.go
   40 stackconf/files.go
  237 stackconf/serialize.go
 1318 stackconf/plan.go
  128 stackconf/staging.go
  255 stackconf/backups.go
  315 stackconf/moved.go
  203 stackconf/job.go
  209 stackconf/export.go
  137 stackconf/deps.go
 1369 stackconf/stackconf.go
  997 stackconf/runner.go
 1986 stackconf/apply.go
  254 stackconf/middlewares.go
   72 staging/staging.go
   94 staging/patch.go
   40 varref/usage.go
  825 varref/varref.go
  143 varref/catalogue.go
```

Severity scale used below, held consistently:

- **bug** — the applier and a human-facing surface (panel / API) disagree, and
  the disagreement has a reachable bad outcome. Reserved for drift.
- **misplaced-owner** — the rule is in the wrong layer, but there is one caller
  and no second spelling, so nothing can drift today.
- **duplication** — the same rule spelled twice, not (yet) drifted.

---

## Findings

### varref/varref.go:520 — `scopeVar`: the env→stack→org variable precedence walk

- **Rule:** `${{ stack.vars.X }}` / `${{ stack.secrets.X }}` resolve by looking
  at the consumer tile's **environment** rows first (`repo.OwnerEnv`,
  :530-542), then the stack (`repo.OwnerStack`, :544). `org.*` skips the env
  rung entirely (:522-524). The reference's bucket must match the row's
  `Secret` flag or it is `errBucket` (:502), not an unset value. An unset name
  is a typed `*UnsetError` (:484) so the deploy engine can park rather than
  fail.
- **Category:** 1 (branching on domain state) + 3 (orchestration) + 6
  (duplication across surfaces).
- **Owner it should have:** `service.VariableService`. `varref` is the unowned
  **read half** of that service: the write half already went behind
  `Ops.Vars.Upsert` / `ReplaceTileVars` / `Remove` (see the doc comment at
  `envops/envops.go:57-60` narrating exactly that move), and the read half —
  which is where the precedence rule actually lives — did not.
- **Also spelled at:** `stackconf/runner.go:474` (`planVars`, env vs stack
  split), `:553` (`SecretIssues`, `envSet` vs `set`), `:590`
  (`DeclaredInputs`, same walk again), `:674` (`setVarNames`, stack∪org),
  `:755` (`checkRef`, stack-or-org owner pick), `stackconf/apply.go:1798`
  (`ensureSecrets`, per-env vs stack-wide mint), `:1861` (`applyVars`),
  `orgconf/orgconf.go:522-557` (org vars/secrets), `varref/usage.go:20`
  (`ScopeVarsUsed`, a deliberately non-resolving fourth reading).
- **Severity:** misplaced-owner — **except** for the checkRef copy below, which
  has already drifted.

### stackconf/runner.go:773 — `checkRef` never reads the env rung — **DRIFT**

- **Rule:** for a bucket reference, `checkRef` picks
  `repo.OwnerStack`/`repo.OwnerOrg` from `ref.Scope` (:778-781) and lists only
  that owner's variables (:782). It never consults `repo.OwnerEnv`.
- **Concrete failure mode — a self-inflicted deadlock, no panel involved.**
  The file declares `environments.<env>.vars: {FOO: bar}`. There is no
  env-scoped reference form in the grammar (`varref.go:11-14`), so the only
  spelling is `${{ stack.vars.FOO }}` — and `planVars` says so itself: its
  change Note is built by `scopeRef` (`runner.go:536-541`), whose doc comment
  reads "an env row still shadows a stack variable, so it is read through
  stack." A tile in that env then writes `${{ stack.vars.FOO }}`:
  1. `checkRef` picks `repo.OwnerStack` (:778), lists stack rows only (:782),
     finds nothing, returns `"FOO has no value; set it in the stack's vars"`
     (:800).
  2. That lands in `plan.Errors` (`runner.go:421`, and again at
     `apply.go:302`).
  3. `apply.go:315` turns a non-empty `plan.Errors` into a hard refusal.
  4. The env row that would satisfy it is written by `applyVars`
     (`apply.go:1902`), which runs inside `execute` — which the error gate at
     step 3 never reaches.

  So the plan's own guidance text ("read as `${{ stack.vars.FOO }}`") and the
  plan's own gate contradict each other in adjacent functions, and the file has
  no way out of the state. The same shape reaches from outside too: any
  env-level row the panel's env Variables tab writes, or `sharelink.Submit`
  with `OwnerKind == OwnerEnv`, resolves fine at deploy
  (`varref.go:530-542`) and reads as missing here.
- **Not affected:** `default: generated` secrets. `BadRefs` builds `declared`
  from `re.Secrets` (`runner.go:730-733`) and `ensureSecrets` mints only names
  from that same map, so they take the exemption at `:775`.
- **Category:** 6 (duplication that has drifted).
- **Owner it should have:** `VariableService.Lookup(ctx, consumer, ref)` — one
  walk, used by both the resolver and the plan gate.
- **Also spelled at:** `varref/varref.go:520`. The escape hatch that hides this
  in the common case is the `declared[ref.Name]` exemption at `:775`, which
  only covers `stack` + **secret** refs declared in the same file.
- **Severity:** **bug**.

### stackconf/runner.go:790 / varref/varref.go:502 — bucket-mismatch message, twice

- **Rule:** naming a secret through `.vars.` (or the reverse) is a config
  fault, not an unset value.
- **Category:** 6.
- **Owner it should have:** same `VariableService` lookup; `errBucket` already
  renders the sentence.
- **Also spelled at:** the two sites differ in wording only
  (`"is a secret at stack level"` vs `"is in the secrets namespace"`).
- **Severity:** duplication.

### varref/catalogue.go:42 — `Catalogue`

- **Rule:** what a tile may reference — siblings in its env minus volumes and
  itself, only **bound** resources ("advertising [unbound ones] would suggest
  references that are guaranteed to fail", :41), the stack's and org's two
  buckets, and the always-present `stackr` scope. Endpoint output names are
  hardcoded here (:67) and again in `varref.go:788-811`.
- **Category:** 1 + 6.
- **Owner it should have:** `VariableService` (autocomplete read path).
- **Also spelled at:** `varref/varref.go:778` (`endpointOutputs` — the real
  values behind the same five names).
- **Severity:** duplication.

### varref/varref.go:70 — `VariableService` owns no read path at all

- **Fact:** `service.VariableService` (`service/variable.go:70`) owns every
  variable **write** and exactly one read — `List` (`:325`), a straight
  `ListVariables` passthrough. Every caller that needs the *resolved* cascade
  goes to `varref` directly, including two handler layers:
  `handlers/web/handler/project/handler.go:1403`,
  `handlers/api/v1/variables.go:158-165` (`Resolve`),
  `handlers/web/handler/app/handler.go:945` and
  `handlers/api/v1/variables.go:321` (`Catalogue`),
  `handlers/web/handler/org/graph.go:547` and
  `handlers/web/handler/project/stackgraph.go:453,507` (`ScopeVarsUsed`).
  `service/` itself touches `varref` only for `OrgStorageRef`
  (`service/storage.go:135`, `service/tilevalidate.go:231`).
- **Verdict:** confirms the thesis. `varref` is the unowned read half of
  `VariableService`, and handlers reach past the service to get it.
- **Severity:** misplaced-owner (the headline structural finding).

### varref/varref.go:89 — `EnvLines` is dead code

- **Rule:** a caller that cannot attach extra networks (compose runs, db tiles)
  must not get a value whose hostname only resolves on a shared overlay.
- **Fact:** zero callers repo-wide. (The `EnvLines` hits elsewhere are the
  unrelated `stackconf.EnvLines`, `stackconf/stackconf.go:1326`.) Either a
  caller lost it — in which case somebody is now starting containers with
  unreachable hostnames — or it should go.
- **Severity:** dead code; worth one grep before the service move.

### varref/varref.go:145 + stackconf/stackconf.go:1006 — reserved slugs: CLEAN

- **Rule:** a tile or shared instance may not be named `vars`, `secrets`,
  `backups` or `storage`.
- **Panel/API:** enforced in one place for both — `service/tile.go:333`
  (`repo.ReservedSlug` inside `nameTile`, from `Create` at `:287`), plus
  `service/managedinstance.go:148` for instances. Panel
  `handler/project/handler.go:502`, API `api/v1/apps.go:85`,
  `api/v1/volumes.go:55` all route through it. Tile **rename** has no panel or
  API route at all, so `TileService.Rename`'s check (`service/tile.go:431`) has
  exactly one caller: `stackconf/moved.go:308`.
- **Verdict:** no drift. `varref.Reserved` (:145) is a one-line wrapper whose
  only caller is `stackconf.go:1006`; the service calls `repo.ReservedSlug`
  directly. Two names for one thing, nothing more.
- **Severity:** clean.

### stackconf/stackconf.go:1003 — `validateTile`: the whole tile grammar

- **Rule:** ~40 domain rules — source required, ingress only for keep-alive
  kinds, `command:`/`schedule:`/`run_on_deploy`/`files:`/`storage:` gated by
  `runpolicy`, restart normalisation, device/dep/file-mount/storage-attachment
  parsing, cron expression validity, engine support, external port range,
  scope, slice address shape, `on_remove`, and the domain entry rules.
- **Category:** 2.
- **Owner it should have:** split. The *shape of the config file* (which keys
  a type accepts) genuinely belongs here. The *domain rules* — cron validity,
  engine support, port range, restart policy, replicas-vs-volume — belong to
  `TileService.Validate`, which the applier already calls afterwards
  (`apply.go:1253`). Today a tile is validated twice by two rule sets and the
  file-side one is the stricter.
- **Also spelled at:** `service.TileService.Validate` (called at
  `apply.go:1253`, `:1472`); `runpolicy/runpolicy.go:29` holds the
  per-kind capability matrix that `validateTile` then re-branches on.
- **Severity:** duplication.

### stackconf/stackconf.go:1354 — `validateReplicas`

- **Rule:** `replicas > 1` is refused on a tile that mounts a volume, using
  the config's own view of "pinned" because the row may not exist yet. The
  comment (:1352) says `infra/placement` is the real classifier.
- **Category:** 2 + 6. **Owner:** `TileService.Validate` (one classifier).
- **Severity:** duplication.

### stackconf/stackconf.go:916 — `validateVars` + :815 secret-name rules

- **Rule:** a var/secret name must be a shell identifier (`envVarRe`, :1170),
  and a name may not appear under both `vars:` and `secrets:`. Plus
  `default:` must be `""` or `generated`, `length` positive, and the
  generation knobs only with `default: generated`.
- **Category:** 2. **Owner:** `VariableService`.
- **Panel/API:** enforces a **different, strictly looser** regex —
  `varNameRe = ^[A-Za-z0-9_][A-Za-z0-9_.-]*$` at `service/variable.go:28`,
  applied in `write` (`:158`), which both `Set` and `Replace` reach. A third
  copy of the looser one sits in the panel at
  `handlers/web/handler/org/settings.go:384`. The both-var-and-secret rule has
  no counterpart at all.
- **Severity:** **bug**, twice — see the divergence table.

### stackconf/stackconf.go:942 + backups.go:45 — backup destination reference rule

- **Rule:** a `backup.dest` must be **exactly** one `${{ org.backups.NAME }}`
  or `${{ stackr.backups.NAME }}` and nothing else, because "a destination
  carries the bucket credentials and the file is in git" (:982). Schedule must
  be a valid cron, tz a real zone, keep non-negative, `mode:` only on a volume
  tile. `backupKind` is derived, never declared (backups.go:36).
- **Category:** 2 + 1.
- **Owner it should have:** the parse-time half is config's. `backupKind` and
  the mode/volume guard belong to `BackupScheduleService` — and the apply side
  **already gets this right**: `backups.go:236` uses `Schedules.Adopt` and
  `:242` `Reconcile`, so `backup.Validate` and the volume guard run. The
  divergence here is on the panel side, already recorded in
  `11-business-logic-sweep.md:96-101`; this shard confirms the applier is the
  correct half, and refines that earlier entry: the panel's
  `settings/handler.go:240-270` raw `Save` is **justified** — it is the panel's
  own *database* backup (`repo.BackupStackr`), a row with no tile, which
  `Create`/`Adopt`/`Reconcile` are all written about
  (`service/backupschedule.go:289-295`), and it does call `ReloadBackups`
  itself at `:267`. The API delete, `api/v1/backups.go:205`, calls
  `schedules.Remove` (the raw store delete, `service/backupschedule.go:309`)
  where the panel calls `Delete` (`:177`), which reloads at `:185`.
  *Corrected 2026-09-21 (`12-business-logic-plan.md` §2 #1):* this entry said an
  API-deleted schedule keeps firing until restart. It does not — `:207` calls
  `a.sched.ReloadBackups` right after. A hand-written re-spelling of `Delete`,
  not a live bug.
- **Severity:** duplication (applier clean, API delete re-spells `Delete`).

### stackconf/plan.go:343 — `claimHost` and the foreign-org-slug refusal

- **Rule:** `auto:` generates under the nearest visible domain resource;
  `apex:` must name a visible one; a **literal** host whose first label is
  another org's slug is refused — "config as code would otherwise be the hole
  in the anti-squat rule the UI paths enforce" (plan.go:312-315, :359).
- **Category:** 2 + 6.
- **Owner it should have:** `DomainService`. Note `claimHost` already delegates
  the *other* two domain rules to services (`service.AutoHost` :350,
  `service.CheckWildcardHTTPS` and `service.DomainPort` at :947/:950) — the
  anti-squat rule is the one that stayed behind as a local `map[string]bool`
  built in `runner.go:147-153`.
- **Panel/API:** the comment is **true**. `service.CheckOrgSquat`
  (`service/domainresource.go:61`) runs inside `DomainService.plan`
  (`service/domain.go:145`), which every domain-add path reaches:
  `handler/app/handler.go:1469`, `api/v1/domains.go:48`,
  `handler/db/handler.go:531`; domain *resources* are squat-checked at
  `service/domainresource.go:274,281,288`. The staged panel branch
  (`app/handler.go:1473-1490`) runs after `Attach` has validated, so staging is
  not a hole.
- **Severity:** duplication — same rule, two data sources (a store read vs a
  precomputed set). Not drift today; collapse when `varref`/domain rules move.

### stackconf/plan.go:922 — `claimHost` (the plan's own host ledger)

- **Rule:** host+path is unique server-wide; a claim another tile holds is a
  plan error, and a host moving between two tiles in one plan is one change,
  not a conflict (`releasedHosts`, :377).
- **Category:** 2 + 3. **Owner:** `DomainService`. The panel/API side gets
  ownership from `DomainService.plan` (`service/domain.go:145` and the checks
  around it); the difference is that the plan needs a *ledger* — it must decide
  about many claims at once, including claims it is about to release — which a
  single-claim service call cannot express.
- **Severity:** misplaced-owner (the batch form belongs beside the single form).

### stackconf/plan.go:911 — `checkConnector`

- **Rule:** a tile's `connector:` id must belong to the stack's own org,
  because that connector mints a GitHub token that clones private repos. The
  `ConnectorsKnown` flag (State:40-45) distinguishes "lookup failed" from "org
  owns none" — a distinction whose absence was the previous bug.
- **Category:** 2 (a real authorization rule).
- **Owner it should have:** a connector/credential service. The comment says
  `infra/githubapp` has a deploy-time guard, i.e. the rule is already in two
  places at two layers.
- **Severity:** duplication (security-relevant).

### stackconf/plan.go:544 vs orgconf/orgconf.go:476 — domain-resource diff, twice

- **Rule:** identical logic at two levels — host uniqueness across
  `AllDomainResources`, `IncludeEnvOnDefault` update, `acme_email` update with
  the "traefik restarts once" note, the `Declared` adoption rule ("added in the
  panel; the file adopts it, and dropping it from the file will delete it"),
  and delete-only-if-`Declared`.
- **Category:** 6. The two copies are ~40 lines each and already differ: the
  stack copy emits the adoption row only when *both* `IncludeEnvOnDefault` and
  `ACMEEmail` already match (plan.go:578); the org copy checks
  `IncludeEnvOnDefault` alone (orgconf.go:504).
- **Owner it should have:** `DomainResourceService.Reconcile(level, ownerID,
  []DomainResConf)` — the apply halves (`apply.go:415`, `orgconf.go:827`) are
  a second matched pair of near-identical functions.
- **Panel/API:** uniqueness **is** enforced, in the service —
  `service.HostTaken` (`service/domainresource.go:47`, used at `:181`) plus
  host shape (`:173`) and squat/config-ownership (`:183`), reached by
  `handler/org/settings.go:492`, `handler/project/handler.go:2270`,
  `handler/server/handler.go:218`, `api/v1/domainresources.go:118`. **One
  bypass:** `handlers/web/handler/org/setup.go:223` writes through the raw
  `resources.Save` (`service/domainresource.go:322`) with no `HostTaken` and no
  squat check, guarded only by an emptiness test at `:220`.
- **`Declared` note:** read in exactly two places
  (`service/domainresource.go:114` for ranking, `stackconf/plan.go:584` for
  "only the file's rows are the file's to delete") and **set true in exactly
  one** — `stackconf/apply.go:442`. No panel or API path ever sets it, which is
  the intended asymmetry, and worth stating so nobody "fixes" it.
- **Severity:** duplication (two copies in this shard, already cosmetically
  drifted) + **bug** for the `setup.go:223` bypass.

### stackconf/apply.go:223 — `PolicyFor` and the whole auto-apply gate

- **Rule:** manual unless the env's own `ApplyPolicy == "auto"`, and
  `protected: true` ratchets back to manual regardless. On top:
  `holdManual` (:754) skips non-auto envs and pins the home env to "apply only
  when some rung does"; `allAuto` (:791) maps the `"stack"` pseudo-env onto the
  default env; and `ApplyPlan:350` refuses an unattended apply that is
  destructive, renames the stack, or carries a `moved:` entry.
- **Category:** 1 + 3. This is the single most consequential decision in the
  shard — it decides whether a webhook may change production unattended.
- **Owner it should have:** a `ConfigApplyPolicy` on the service side, or at
  minimum `PolicyFor` exported (it already is) and used by every surface that
  answers "will this apply on its own?".
- **Severity:** misplaced-owner.

### stackconf/apply.go:238 — `ApplyPlan` orchestration

- **Rule:** a 150-line sequence with a decision between nearly every step:
  scope → load at the reviewed **sha** not the branch tip (:248) → hold manual
  envs → diff → re-run planRename/planVars/planBackups (:290-298, "without them
  a commit that only touches vars: diffs to nothing and is recorded applied")
  → re-run `BadRefs` (:302, "the webhook path plans and applies in one go, so a
  plan-only gate never gated that flow") → mint secrets **before** the gate
  (:306) → error gate → pending-move gate → slice preflight → `ui_edits` write
  positioned deliberately between two gates (:329-336) → empty-plan short
  circuit → force gate → domain resources → middlewares → rename → strip the
  synthetic vars/backup rows (:377) → execute → finish middlewares → status.
- **Category:** 3. Every one of those ordering constraints is load-bearing and
  documented as a past bug.
- **Owner it should have:** this one is arguably correctly placed — it is *the*
  config engine. The finding is that it reaches **through** `Ops` into six
  services and one infra package rather than being called by a service.
- **Severity:** misplaced-owner (low priority; do not move casually).

### stackconf/apply.go:1700 / serialize.go:76 / staging/patch.go:35 — three hand-kept mirrors

- **Rule:** the TileConf↔repo.Tile field mapping, written out three times:
  `applyTileConf` (write side), `tileToConf` (read side), `SettingsPatch`
  (staged-patch side). `patch.go:32` states the requirement outright: "it has
  to stay in step with stackconf.tileToConf, which is the same mapping in the
  other direction", and explains it cannot just marshal a TileConf because
  every field is `omitempty`.
- **Category:** 6. A field added to one and not the others silently stops
  round-tripping — `serialize.go:19` names the round-trip test as the only
  definition of correct.
- **Owner it should have:** one mapping, generated or table-driven, owned
  beside `repo.Tile`.
- **Severity:** duplication (structurally guaranteed to drift).

### stackconf/apply.go:1798 — `ensureSecrets`

- **Rule:** mint `default: generated` secrets that have no value; per env when
  `env_versions` (the default), one stack-wide row otherwise; never rotate an
  existing value at any layer; audit-record the mint; clear tiles parked
  waiting on that name (`deploy.ClearWaitingEnv` / `ClearWaiting`).
- **Category:** 1 + 5 (side effects fired by a caller).
- **Owner it should have:** `VariableService` — it already owns the write
  (`Ops.Vars.Upsert`, :1835) but not the "which owner, and then clear the
  waiters and write the audit row" decision around it.
- **Also spelled at:** `orgconf/orgconf.go:743-757` (org-level generated
  secrets: same mint, same `audit.Record`, same `deploy.ClearWaitingOrg`).
- **Severity:** duplication.

### stackconf/apply.go:1861 — `applyVars` / runner.go:467 `planVars`

- **Rule:** vars are additive + update, never deleted ("nothing records where a
  variable came from"); a name already stored as a **secret** is skipped on
  apply and errored on plan; each write clears the waiters and records audit.
- **Category:** 1 + 5. **Owner:** `VariableService`.
- **Also spelled at:** `orgconf/orgconf.go:530-538` and `:730-737`.
- **Severity:** duplication.

### stackconf/apply.go:1963 / plan.go:1310 — pinned-tile group move

- **Rule:** the plan blocks on a `node_group` change for a tile with a
  `HomeNode`; the applier then clears blocks the operator has dealt with using
  `placement.InGroup`. The comment (:1948-1959) documents that without the
  applier half the block could *never* be cleared.
- **Category:** 1 + 4 (direct infra use). **Owner:** a placement/volume-move
  service. **Severity:** misplaced-owner.

### stackconf/apply.go:1374 — `waitDeps`

- **Rule:** blocks the apply walk on `healthy`/`completed` dependencies with a
  per-dependency deadline stretched by the target's start period, short-circuits
  on `deploy.WaitingFor`, polls every 2s. Explicitly not done in the deploy
  worker because that FIFO would deadlock (:1370-1373).
- **Category:** 3 + 4. **Owner:** the deploy orchestration layer.
- **Severity:** misplaced-owner.

### stackconf/apply.go:1163 — `refBroken`

- **Rule:** decides whether to redeploy a tile after a slice lands by
  **string-matching** the last deployment's error against
  `"no tile or resource named"`.
- **Category:** 1. This is a domain decision keyed on a message produced in
  `varref/varref.go:590` — two files that must agree on a sentence with nothing
  linking them.
- **Owner it should have:** a typed error (like `varref.UnsetError` already is)
  plus a `DeployService` predicate.
- **Severity:** **bug** risk — reword the message in varref and this silently
  stops healing. Not a surface divergence, so filed as duplication-by-string.

### stackconf/slices.go:21 — `resolveFrom`

- **Rule:** the narrowest-wins resolution order for a slice's `from:` address —
  own env, then stack scope, then org scope; the 2-segment form tries
  `org:stack:instance` then `org:instance`.
- **Category:** 2 + 3. **Owner:** `managedtiles` / a slice service (it already
  calls `managedtiles.ResolveInfraPath`). **Severity:** misplaced-owner.

### stackconf/slices.go:179 — `syncBindings`

- **Rule:** a tile is granted a binding to every env resource its variables
  reference. It **grants and never revokes** — the doc comment claims "and
  revokes what it no longer references" (:174) but the loop only calls
  `CreateBinding`; there is no delete path — although
  `repo.Store.DeleteBinding` exists (`store/repo/repo.go:298`,
  `store/sqlite/variables.go:137`), so this is an omission, not a missing
  capability.
- **Category:** 2 (this is an access grant). A stale binding means
  `varref.resourceOutput` (:674) keeps resolving a resource the file no longer
  references.
- **Owner it should have:** a resource-binding service.
- **Severity:** **bug** — the comment and the code disagree about a permission.

### stackconf/moved.go:41 — `ParseMoves` / :115 `PlanMove`

- **Rule:** a rename is never inferred; both sides must be the same kind; no
  self-move; no duplicate from/to; no chains (a→b, b→c). `PlanMove` then
  decides between change / warning / error on `HasFrom`/`HasTo`/`Declared`.
- **Category:** 1 + 2. Correctly factored: `orgconf/moved.go:18` reuses both
  with its own kinds (`stack`, `shared`). This is the shard's **best** example
  of a shared rule.
- **Severity:** clean.

### stackconf/moved.go:255 — `applyMoves` / orgconf/moved.go:61

- **Rule:** teardown-then-rename-then-restart, because swarm cannot rename a
  service. The org copy does the same for shared instances but writes the row
  with `RenameTile` and restarts via `Applier.RestartTile`.
- **Category:** 3 + 5. The stack copy goes through `Ops.Tiles.Rename` /
  `Ops.Envs.Rename` (correct); the **org** copy at `orgconf/moved.go:97-107`
  calls `envnet.TearDown` and `Store.RenameTile` directly — infra and store,
  no service.
- **Severity:** duplication, with the org half bypassing the owner.

### stackconf/middlewares.go:105 — `diffMiddlewares` / :158 `checkMiddlewareRefs`

- **Rule:** a middleware still referenced (by this file or by another stack's
  domain rows) cannot be deleted — "traefik disables a router naming a
  middleware that does not exist, and says so only in its own log" (:117).
  Cross-stack references are resolved against the org (`OrgMiddlewares`).
- **Category:** 1 + 2. Proxy-owned rules living in the config parser.
- **Owner it should have:** `service/proxy`. `applyMiddlewares` (:191) already
  calls `Ops.PX.SyncStackMiddlewares`, so only the rules stayed behind.
- **Severity:** misplaced-owner (no second surface — middlewares are file-only,
  which is itself worth noting: there is no panel path to drift from).

### stackconf/apply.go:486 — `applyUIEdits`

- **Rule:** resolves the `ui_edits` mode: this file → the org file's
  `defaults:` → `block`. Written onto the stack row "so handlers read one
  column instead of the config".
- **Category:** 1 + 3. **Owner:** `StackService`. **Severity:** misplaced-owner.

### stackconf/apply.go:955-1012 — the "declaring nothing ≠ declaring inherit" rule

- **Rule:** settings, colour and apply policy are written **only** when the
  file declares them; position is written unconditionally because it is the
  file's declaration order. The comment (:983-994) records this as a fixed bug
  — colour and policy used to be stamped on every apply, reverting panel and
  API edits. A `settingsChanged` flag then fires `Ops.PX.Resync`.
- **Category:** 1 + 5. **Owner:** `EnvironmentService` / `SettingsService`.
- **Severity:** misplaced-owner (the rule is correct and subtle; it needs a
  home where the panel can read it).

### stackconf/runner.go:70 — `dnsProvider`

- **Rule:** whether `*.example.com` is accepted at all depends on the DNS-01
  provider setting, read through `svcproxy.New(...).DNS(ctx)`.
- **Category:** 1. Correctly routed through the service — and the comment
  (:60-66) records that this was the fix for the key being typed by hand in two
  packages. Keep as the template for the rest.
- **Severity:** clean.

### stackconf/runner.go:986 — `Replan` fires a background goroutine

- **Rule:** a change elsewhere triggers a whole-stack replan on
  `context.Background()`.
- **Category:** 5 (side effect fired by a caller). It satisfies
  `service.Replanner`, so the service layer calls *down* into this — the right
  direction. **Severity:** clean, noted for completeness.

### stackconf/plan.go:1247 `Summary` / :295 `Declared` / :219 `InputsFirst`

- **Rule:** declared-but-unset secrets are shown and never counted, "a stack
  whose only row is an unset secret has nothing to apply, and counting it left
  that stack with a plan pending review forever". `GenSecrets` **do** count.
- **Category:** 1. **Owner:** `PlanService` (which already owns the row —
  `runner.go:55`). **Severity:** misplaced-owner.

### orgconf/orgconf.go:450 — org rename gated on registry images

- **Rule:** an org that has ever pushed an image cannot be renamed, because the
  registry namespace is the org slug and a docker registry has no rename.
  Checked at plan time (:455) **and again at apply** (:690) "not only in the
  plan: an apply that skipped its plan would otherwise orphan every image".
- **Category:** 1 + 2. Correctly routed through
  `service.RegistryService.OrgHasImages` (:281).
- **Panel:** enforces the same, through the same service —
  `handlers/web/handler/org/members.go:110-117`, which also squat-checks the
  new slug at `:99-102`. There is **no org-rename endpoint in the JSON API**
  (`api/v1/v1.go:369-392` registers list, settings, members, invites only).
- **Severity:** clean — one of the few rules this shard found enforced
  identically on every path that exists.

### orgconf/orgconf.go:92 — `StackRef.Protected` is parsed and never read — **DEAD RULE**

- **Rule as documented:** "Protected ratchets the delete side: removing this
  stack from the file is refused outright instead of planned."
- **What actually happens:** `UnmarshalYAML` peels and stores it
  (:120-126). The plan's deletion loop (:631-640) checks only `declared[slug]`
  and `s.ConfigManaged()`. The apply loop (:794-810) checks the same two and
  carries a comment *rationalising* the omission ("protection lived in the file
  that declared them, so absence means the operator already removed the flag
  too; the manual approve is the gate"). Nothing else in the package reads the
  field.
- **Concrete failure mode:** an operator writes `protected: true`, gets no
  error, and believes the stack cannot be deleted by a config change. Removing
  the stanza plans and applies a full `TeardownStack` + `DeleteStack`.
- **Category:** 2. **Owner it should have:** `StackService` (a `protected`
  column, the way `EnvConf.Protected` is a real ratchet at `apply.go:768`).
- **Confirmed:** read nowhere in non-test code. Note this is a *different*
  field from `stackconf`'s `EnvConf.Protected` (`stackconf.go:159`, `:591`),
  which is live and correct (`stackconf.go:1302`, `apply.go:770`, `:814`,
  `export.go:44`) — so the name collision makes the dead one look wired.
- **Severity:** **bug** — a safety flag that silently does nothing. Either wire
  it into the deletion filter at `orgconf.go:797` or delete the field; leaving
  it parsed-and-ignored is the worst of the three.

### orgconf/orgconf.go:412 — `markDeclared` writes rows at **plan** time

- **Rule:** every plan (including webhook-triggered ones) rewrites
  `Stack.OrgDeclared` on every stack in the org, via a raw
  `Store.UpdateStack`. That flag is what `runner.go:452` gates stack renames on.
- **Category:** 1 + 5. A plan is supposed to be a read; this one mutates rows
  that another gate then reads. `PreviewBundle` (:371) deliberately skips it
  "preview must write nothing, and that refresh mutates stack rows" — which
  concedes the point.
- **Owner it should have:** `StackService`. **Severity:** misplaced-owner
  (plan-time write is the smell; single caller, so not drift).

### orgconf/orgconf.go:956 — `applyShared`

- **Rule:** reconciles org-scoped managed instances: same-engine → in-place
  image/shm/port update + redeploy when `running`/`error`; engine change →
  full teardown then recreate; undeclared → teardown with `force`. New rows are
  built **by hand** here (:1006-1014) — `uuid`, webhook token, `ScopeKind`,
  `managedtiles.NewDB`, `Store.CreateTile`, `PublishConnection` — where the
  stack-side equivalent goes through `createTile` → `Ops.Tiles.Validate`.
- **Category:** 1 + 3 + 4.
- **Owner it should have:** `ManagedInstanceService` (it already owns
  `Deploy`/`TearDown`, used at :986 and :1042 — only the create stayed behind).
- **Severity:** misplaced-owner; note the asymmetry with `apply.go:1253`, where
  the stack applier *does* call `Tiles.Validate` and this path does not.

### orgconf/orgconf.go:1048 — `hostEnv`

- **Rule:** an org-scoped instance's row physically lives in the org's oldest
  stack's first environment. Self-described as "arbitrary but stable".
- **Category:** 1. **Owner:** `ManagedInstanceService`.
- **Severity:** misplaced-owner (and a latent bug: deleting that stack moves
  every org instance's home).

### orgconf/storage.go:41 — `validateStorage`

- **Rule:** an org share is `smb` or `nfs` only — `local` is refused by name
  with an explanation ("local pools belong to a server"); slug shape enforced;
  address+export required; credentials must be `${{ org.vars|secrets.NAME }}`
  and nothing else.
- **Category:** 2. **Owner:** `service.StorageService`, which exists.
- **Panel/API:** `local` is refused for an org share at `service/storage.go:75`
  and the backend set is gated at `:61`; address is required at `:81`. But
  **`export` is not required** for an nfs/smb org share — `:78` only demands an
  absolute `Export` for a *local* pool — while the config file requires both
  (`storage.go:53-55`). An API-created org share with an empty export is
  accepted and fails later, at probe. (Credential-must-be-a-reference is
  correctly config-only: that rule exists because the file is in git;
  `StorageSpec.Username/Password`, `service/storage.go:50-51`, take literals by
  design. The panel has no org-share create at all —
  `handler/org/settings.go:443-460` is read-only; the only panel create is the
  server pool at `handler/server/storage.go:50`.)
- **Severity:** **bug** (minor) on `export`; the rest clean.

### orgconf/storage.go:73 — `StorageConf.want` re-implements reference expansion

- **Rule:** expands `${{ ... }}` in the username/password against the org's
  variable rows, using its **own** `refRe` (storage.go:39) — a byte-identical
  copy of `varref.refRe` (varref.go:109) — and its own lookup that ignores
  scope, bucket and the secret flag, returning `""` for a missing name and
  collecting it into `missing`.
- **Category:** 6. A fourth resolver, in a file that already imports `varref`
  for `Refs`/`Parse`.
- **Owner it should have:** `VariableService` / `varref` — the same walk as
  every other reference.
- **Severity:** duplication (and it silently disagrees with `varref` about
  bucket/secret matching, which `validateStorage` only partly covers).

### orgconf/storage.go:176 — share delete refused while mounted

- **Rule:** a share still named by any tile's `storage:` lines in the org
  cannot be dropped; the error lists the mounting tiles.
  `shareUsers` (:116) walks every stack × tile using `varref.OrgStorageRef`.
- **Category:** 1 + 2 + 6. **Owner:** `StorageService`, which **already has
  this rule**: `Delete` → `Consumers` (`service/storage.go:155-166`, `:125`)
  matches org shares with the same `varref.OrgStorageRef` at `:135`. Panel
  `handler/server/storage.go:82`, API `api/v1/storage.go:138`. Sub-path delete
  deliberately skips it (`service/storage.go:261`; both surfaces confirm
  first).
- **Severity:** duplication — `orgconf.shareUsers` is a second implementation
  of `StorageService.Consumers`. Same answer today; delete one.

### orgconf/storage.go:195 — `applyStorage` drops volumes before rewriting

- **Rule:** changing a share's backend/address/credentials calls
  `storagetiles.DropOrgShareVolumes` before the row update, and the same on
  delete.
- **Category:** 4 (direct infra use from a config applier).
- **Owner it should have:** a storage service. **Severity:** misplaced-owner.

### orgconf/orgconf.go:161 — `Parse` validates an embedded stack file

- **Rule:** an inline stack body inherits the org file's `version:`, takes its
  name from the org key and may not disagree with it, and is fully
  `stackconf.Load`-ed at parse time so "a broken inline stack must fail the org
  plan, not the apply".
- **Category:** 2. Correctly delegated to `stackconf` — no second grammar.
- **Severity:** clean.

### orgconf/orgconf.go:661 — `Apply` ordering

- **Rule:** moves → rename → org row defaults → vars/secrets → domains →
  storage → stacks (with a **second pass** for stacks that failed on a missing
  middleware, :775-788) → stack deletions → shared instances. Failures are
  collected, not fatal.
- **Category:** 3. The "retry once if the error message contains
  `no middleware `" branch (:777) is string-matching on
  `middlewares.go:177`'s wording — the same class of fragile coupling as
  `refBroken`.
- **Owner it should have:** the ordering is config's; the string match is a bug
  waiting to happen. **Severity:** duplication-by-string.

### orgconf/export.go:22 / stackconf/export.go:191 — export

- **Rule:** never write a secret's value into a file destined for git; an org
  share's password is replaced by a reference to the secret that holds it, or a
  new secret is declared for the operator to fill (export.go:61-72); a
  panel-managed stack is deliberately **not** declared in the org file.
- **Category:** 1 + 2. Correct and well-reasoned; the round-trip test is the
  stated contract. **Severity:** clean.

### envops/envops.go:30 — the whole package

- **Rule:** `CloneTiles` (:77) — fresh identity per tile, regenerated db
  credentials with a value-rewrite pass over sibling env blobs, `SharedNetName`
  and `HomeNode` cleared "both belong to the environment the tile came from",
  fresh empty volumes, auto-domain inheritance only when the base carried one,
  per-consumer slice re-provisioning (`cloneProvisions`, :277) with reference
  repointing (`repointRefs`, :305), env-level slice cloning (:198), layout copy
  filtered to slug-keyed nodes (:167). `Teardown` (:338) — per-tile envnet
  teardown, proxy drop, slice reclaim with `ephemeral ⇒ drop / static ⇒
  detach`, env-variable cleanup ("variables have no FK to environments… or they
  leak forever"), pooled-overlay release ordered **before** the row delete with
  a paragraph explaining that getting it wrong hands a live network to another
  org, then `Envs.Remove` and `Sched.Reload`.
- **Category:** 3 (orchestration), 4 (direct infra), 5 (side effects), 1.
- **Owner it should have:** `EnvironmentService` — and here the ownership is
  already *nominally* in place: `EnvironmentService.Adopt`
  (`service/environment.go:133`) calls `CloneTiles`, `Delete`/`Reset` call
  `Teardown` (`:216`, `:196`), and `StackService.Delete` (`service/stack.go:174`)
  calls `TeardownStack`. Both are declared as interface methods
  (`service/environment.go:22`, `service/stack.go:20`). So `envops` is the
  **body** of three service methods living one layer below them, reachable
  without them.
- **Bypasses:** `handlers/web/handler/prhook/handler.go:734` calls
  `Ops.Teardown` directly (PR-env teardown, skipping `EnvironmentService`), and
  `:567` calls `envops.CopyLayout` as a package function. Config's own calls
  (`stackconf/apply.go:1593`, `orgconf/orgconf.go:803`) are in-layer and fine.
- **Why it still matters:** `Ops` carries six service pointers (`Tiles`, `Domains`,
  `Resources`, `Envs`, `Vars`, `Sched`, :41-65) and each one's doc comment
  narrates the bug caused by *not* going through the service — "the config
  applier ended up skipping the reserved-slug and duplicate-name checks",
  "cloning an env copied its variables with a raw upsert, so none of the secret
  auditing or the waiting-value clearing happened". The remaining logic is what
  never got a service to move into.
- **Also spelled at:** the `env.Type != "ephemeral"` slice-reclaim branch is
  written twice **inside this one file** — `:357-368` (env-level) and
  `:454-464` (per-tile) — identical six-line bodies.
- **Severity:** misplaced-owner (large) + **bug** for the prhook bypass: a PR
  env torn down through `handler.go:734` misses whatever
  `EnvironmentService.Delete` does around `Teardown`.

### envcompare/envcompare.go:49 — `Compare`

- **Rule:** stack-scoped instances are dropped before comparing because they
  "live in exactly one env by design: not a difference" (:54); domains are
  excluded from the flattened keys ("hostnames differ per environment by
  construction"); a slice's `name` is excluded because it carries the env; any
  value containing `${{` compares as the literal string `"set"` so a secret is
  compared by presence (:215); a key declared by the file on the **reference**
  env with an empty intended value is intended everywhere (:95).
- **Category:** 1 + 2. Real domain rules about what counts as drift.
- **Owner it should have:** a compare/drift service, or at minimum beside
  `stackconf.StateToResolved` which it already depends on (:66).
- **Severity:** misplaced-owner (single caller — the compare panel).

### envutil/envutil.go — clean

Pure parsing/serialisation of KEY=VALUE blobs. `Inject` (:60) has one policy —
never clobber a different existing value, suffix `_2`, `_3` instead — which is
a convention rather than a domain rule, and it is used by exactly one concept
(publishing a connection into a consumer's env). Leave it.

### runpolicy/runpolicy.go:29 — the runnable-type registry

- **Rule:** the per-kind capability matrix (`KeepAlive`, `RequiresSchedule`,
  `AllowsCommand`, `AllowsIngress`).
- **Verdict:** this is exactly the right shape and the right place — it is the
  *replacement* for scattered `if kind == "cron"` branches. The finding is that
  `stackconf.validateTile` (:1017-1097) only half-uses it: it reads `pol` for
  ingress/command/schedule and then writes four more hand-rolled
  `tc.Type != "service"` branches (:1029, :1049, :1063, :1072) for keys the
  registry could have described.
- **Severity:** duplication (small, cheap to close).

### secrets/secrets.go — clean

Crypto and credential generation. `Generate` (:138) carries one domain-shaped
decision — the symbol set is "shell-, URL- and YAML-safe" because the values
land in connection strings assembled by apps that never quote (:144). That is a
correct comment on a correct constant. The panic-instead-of-error contract
(:122-126) is deliberate and right.

### settings/keys.go — clean, and it is only keys

Constants plus one key-builder (`PRPlanKey`, :54). No rules, no store access.
Confirms the task's hypothesis. Its header comment (:8-20) is also the best
statement in the package of *why* the consolidation mattered — `dns_provider`
being read by two packages with nothing linking them.

### settings/settings.go — NOT only keys

Same package, different file, and it carries real cascade rules:

- **`Resolve` (:101)** — later level wins; `ProtectUser`/`ProtectPassword`
  travel as one unit so "a level setting only the user must not pair it with a
  password from the level above" (:122).
- **`Check` (:242)** — refuses half a basic-auth pair, because the resolved
  half-pair makes `proxy.WriteApp` lock every URL below behind a password
  nobody knows.
- **`Merge` (:186)** — the "submitted vs absent vs empty" semantics per field,
  including the three-way special cases for `protect`, `protect_password`
  (blank means keep), `node_group` (empty is a real "any") and `build_node`.
- **`Levels`/`TryChain` (:365, :435)** — the store-error rule: swallow it for
  limits, propagate it for `Protect`, because "a swallowed error resolves to
  'not protected' and publishes the URL" (:340).
- **Category:** 1 + 2. **Owner:** `service.SettingsService`, which exists
  (`service/settings.go:26`) and is a genuine chokepoint: `settings.Merge` has
  exactly **one** caller repo-wide — `service/settings.go:46` — immediately
  followed by `Check()` at `:47`, and every panel and API settings form goes
  through `SaveServer`/`SaveOrg`/`SaveStack`/`SaveEnv` (`:66`, `:84`, `:101`,
  `:119`): `handler/server/handler.go:333`, `handler/org/registry.go:255`,
  `handler/project/handler.go:911`, `:2320`, `api/v1/settings.go:128-134`.
  Handlers import `config/settings` directly only for reads and rendering.
- **Verdict:** no drift. The finding is purely one of altitude — a
  `url.Values` form binder sitting two layers below the only form that uses it,
  with its single consumer one layer above.
- **Positive note:** `stackconf.DefaultsConf.Check` (stackconf.go:220) routes
  the config path straight into `settings.Check`, so that one rule is shared
  rather than duplicated — the model for the rest.
- **Severity:** misplaced-owner; `Merge`'s `url.Values` signature is a form
  binder sitting two layers below the form.

### sharelink/sharelink.go:42 — a documented, unfixed authorization bug

- **Rule the file states about itself (:42-46):** "This package minted links and
  RevokeService revoked them. One concept, two owners, and the mint side
  carried no rules — **which is how a scope granted at mint time still binds
  against whatever org the cookie held**. Moving the writes behind the service
  does not fix that; it puts the mint and the revoke in one place, which is
  where the fix goes."
- **Category:** 2 (authorization). The scope a link grants is not re-checked
  against the org the caller actually holds at mint time.
- **Owner it should have:** the link/revoke service named in the comment.
- **Callers:** handlers only, directly, with no service between —
  `handler/sharepub/handler.go:45,56,68,77,84,111` and
  `handler/project/handler.go:2180,2204,2231`. The sole service-layer touch is
  the authorization verb `VerbShareLinkMint` (`service/access.go:155`), i.e.
  "may this caller mint", not "what may the link it mints reach" — which is
  exactly the gap the header comment describes.
- **Severity:** **bug** (pre-existing, self-documented, still open).
  *Corrected 2026-09-21 (`12-business-logic-plan.md` §4, B18): not reproduced.
  Mint is gated on the org of the stack in the URL (`server.go:533` →
  `orgctx.go:407-408` → `tenancy.go:96`) and the link's owner is that same
  stack. The header comment predates point 18's resource-org gate; it is stale.*

Other rules in the same file, all correctly placed and worth keeping intact if
it moves: GET never mutates (:95), every burn is a guarded UPDATE claim (:8),
`ErrDead` never distinguishes why (:31), all-fields-required on a drop box
(:143), burn+writes in one transaction (:166), values read live at reveal so
revoking leaves nothing to clean up (:180), a grace window defers the burn
(:187). One acknowledged gap at :148: a drop-box submit kicks off **no replan**,
so a config-managed stack's pending-changes view sits stale.

### staging/staging.go:39 — `Stage`

- **Rule:** one row per (env, tile_slug, summary); re-staging a group discards
  the prior row (last-write-wins per group). The payload grammar (`op` +
  `patch`) is config's.
- **Category:** 1. Correctly split already — `Staged` is an interface because
  `service.TileService` owns the row.
- **Callers:** both layers — `service/tile.go:86` and two sites in
  `service/managedinstance.go`, plus handlers directly at
  `handler/app/handler.go:721,1038,1081,1174,1614` and
  `handler/project/handler.go:591`. The direct handler calls are for patch
  shapes the service cannot build, because it cannot import the config engine
  (stated at `handler/app/handler.go:1605-1607`), and each runs after the
  corresponding service call has validated.
- **Severity:** clean. The `staging.Stage` / `staging.SettingsPatch` split is
  the shard's second-best example of a correct layering decision — the payload
  grammar is config's, the row is the service's, and the interface says so.

### staging/patch.go:35 — `SettingsPatch`

- Covered above under the three-mirrors finding. Its own comment (:27-30)
  explains it moved here because "the API stages too now… and two builders
  would be two answers to 'what did this edit change'" — the right instinct,
  landed one layer too low.

---

## Applier-vs-panel divergences

Both columns verified by reading the files. Paths are relative to
`internal/stackrd/`.

### Drift — flagged **bug**

| Rule | Applier file:line | Panel/API file:line or NOT ENFORCED | Difference |
|---|---|---|---|
| Variable name shape | `config/stackconf/stackconf.go:1170` — `^[A-Za-z_][A-Za-z0-9_]*$`, applied at `:816`, `:918` | `service/variable.go:28` — `^[A-Za-z0-9_][A-Za-z0-9_.-]*$`, applied in `write` at `:158` (both `Set` and `Replace`). Third copy of the loose one at `handlers/web/handler/org/settings.go:384` | The surfaces accept `9FOO`, `MY.VAR`, `my-var`; the file can never hold them. On a config-managed stack that is a value with no representation in its own config, and in a container a name no shell can read. |
| A name may not be both a var and a secret | `config/stackconf/stackconf.go:922`; apply-side at `runner.go:472`, `apply.go:1855` | **NOT ENFORCED** — `service/variable.go:156-203` upserts `Secret: w.Secret` at `:196` with no check against the stored row, and `UpsertVariable` keys on (owner, name) only (`store/repo/sqlite/variables.go:36`) | A panel or API write of a plain var over an existing **secret** silently declassifies it to plaintext. Worst outcome in this shard. |
| Bucket ref resolves against the env rung | `config/varref/varref.go:530` (resolver: env → stack); `config/stackconf/runner.go:536` tells authors to spell it `${{ stack.vars.X }}` | `config/stackconf/runner.go:778` (plan gate: stack or org, never env) | Deadlock with no panel involved: a file declaring `environments.<env>.vars` cannot apply, because the gate at `apply.go:315` fires before `applyVars` (`apply.go:1902`) can write the row that would satisfy it. |
| `protected: true` on an org-declared stack | `config/orgconf/orgconf.go:94` — parsed at `:125` | **NOT ENFORCED** — deletion filters at `:631-640` and `:793-810` check `declared` + `ConfigManaged()` only; no other reader | A safety flag accepted without error that prevents nothing. Removing the stanza plans and applies `TeardownStack` + `DeleteStack`. |
| Resource binding revoked when a reference is dropped | `config/stackconf/slices.go:174` comment claims it | the loop (`:194-202`) only calls `CreateBinding`; `repo.Store.DeleteBinding` exists (`store/repo/repo.go:298`) and is never called here | A grant that is never withdrawn. `varref.go:674` keeps resolving a resource the file no longer references. |
| Share-link scope bound to the caller's org at mint | `config/sharelink/sharelink.go:65` | `config/sharelink/sharelink.go:42-46` states it is not; handlers call `Mint` directly (`handler/project/handler.go:2204`) with no service between | Self-documented, still open: "a scope granted at mint time still binds against whatever org the cookie held". |
| Backup schedule delete reloads the scheduler | `config/stackconf/backups.go:216-219` (delete) inside a walk that ends in `Sched.ReloadBackups` (`apply.go:1028`) | panel `handler/backups/handler.go:250` → `Delete` (`service/backupschedule.go:177`, reloads at `:185`); **API** `api/v1/backups.go:205` → raw `Remove` (`service/backupschedule.go:309`) | ~~An API-deleted schedule keeps firing until restart.~~ **Corrected 2026-09-21:** `:207` reloads the scheduler. Re-spelling, not drift (`12-business-logic-plan.md` §2 #1). |
| Domain-resource host uniqueness + squat | `config/stackconf/plan.go:554`, `config/orgconf/orgconf.go:491` | `service/domainresource.go:181` (`HostTaken`), `:183` — reached by 4 of 5 create paths; **`handlers/web/handler/org/setup.go:223` bypasses via raw `Save`** (`service/domainresource.go:322`) | The org-setup wizard can plant a host that collides or squats another org's slug, which every later plan then reports as un-fixable. |
| Org share requires `export` | `config/orgconf/storage.go:53-55` — address **and** export | `service/storage.go:78` requires an absolute `Export` only for a *local* pool; nfs/smb org shares pass with it empty | An API-created org share with no export is accepted and fails at probe instead of at create. (API path: `api/v1/storage.go:90-118`; the panel has no org-share create.) |
| Config applier validates the tile it creates | `config/stackconf/apply.go:1253`, `:1472` call `Ops.Tiles.Validate` — **correct** | **`config/orgconf/orgconf.go:1006-1016`** builds an org-scoped instance row by hand and calls `CreateTile` with no `Validate` | The org applier is the one tile-create path in the repo that skips the shared validator. |
| PR-env teardown goes through the service | `service/environment.go:216` (`Delete` → `teardown`) | `handlers/web/handler/prhook/handler.go:734` calls `envops.Ops.Teardown` directly; `:567` calls `envops.CopyLayout` directly | A handler reaching past `EnvironmentService` into the layer below it. |

### Same rule, one spelling each side — clean

| Rule | Applier file:line | Panel/API file:line | Note |
|---|---|---|---|
| Reserved tile/instance slug | `config/stackconf/stackconf.go:1006` | `service/tile.go:333`, `service/managedinstance.go:148` | Both sit on `repo.ReservedSlug`. Tile **rename** has no panel or API route, so `service/tile.go:431` has one caller: `config/stackconf/moved.go:308`. |
| Literal host starting with another org's slug | `config/stackconf/plan.go:359` | `service/domainresource.go:61` via `service/domain.go:145` | Comment at `plan.go:312` is accurate. Two data sources, one rule. |
| Wildcard host needs a DNS-01 provider | `config/stackconf/plan.go:947` | `service/domain.go:128` | Same function, `service.CheckWildcardHTTPS`. |
| Domain port resolution | `config/stackconf/plan.go:950`, `apply.go:1658` | `service/domain.go:111` | Same function, `service.DomainPort`. |
| Org rename blocked by registry images | `config/orgconf/orgconf.go:455`, `:690` | `handlers/web/handler/org/members.go:110-117` | Same service (`RegistryService.OrgHasImages`). No API org-rename route exists. |
| Org share backend must not be `local` | `config/orgconf/storage.go:48` | `service/storage.go:75` | |
| Share delete blocked while mounted | `config/orgconf/storage.go:186` (`shareUsers`, `:116`) | `service/storage.go:155-166` (`Consumers`, `:125`) | Same answer, two implementations — both match org shares with `varref.OrgStorageRef`. Collapse into `Consumers`. |
| Settings cascade write | `config/stackconf/stackconf.go:220` (`Check` only; config writes `SettingsJSON` straight, `:217-219`) | `service/settings.go:46` — the **only** caller of `settings.Merge`, with `Check()` at `:47` | Every form on both surfaces goes through `SettingsService`. |
| Env colour / apply policy not stamped when undeclared | `config/stackconf/apply.go:988-994` | panel + API write them freely | Correct *because* the applier was fixed; listed so it is not re-broken. |
| `Declared` on a domain resource | set only at `config/stackconf/apply.go:442`; read at `config/stackconf/plan.go:584` and `service/domainresource.go:114` | never set by panel or API | Intended asymmetry — "only rows the file created are the file's to delete". Do not "fix". |

---

## Clean files

Entries marked ↑ have their reasoning in the Findings section above; they are
listed again here only so this list is complete.

- ↑ `envutil/envutil.go` — pure blob parsing; `Inject`'s no-clobber policy is a
  convention, not a domain rule.
- ↑ `secrets/secrets.go` — crypto and credential minting. Correct panic
  contract, correct charset reasoning.
- ↑ `settings/keys.go` — constants and one key builder. Confirms the task's
  hypothesis: **only keys**. (`settings/settings.go` is a different matter, see
  above — do not read "settings is clean" as covering both files.)
- ↑ `runpolicy/runpolicy.go` — the right abstraction in the right place; only
  under-used by `validateTile`.
- `stackconf/files.go` — a YAML unmarshaller for two spellings of one list.
- `stackconf/deps.go` — pure graph validation and topological ordering. Shared
  correctly between the file path (`resolve()`) and the staged path
  (`ApplyResolved:679`).
- `stackconf/moved.go` (the `ParseMoves`/`PlanMove` half) — the shard's best
  example of a rule factored once and reused by both file types.
- `stackconf/job.go`, `orgconf/job.go` — queue wiring, dedupe keys and
  restart semantics (`Requeue` for a convergent apply, `Fail` for a promote).
  The reasoning at `job.go:143-148` is exactly right and belongs where it is.
- `stackconf/export.go`, `orgconf/export.go` — round-trip serialisation with a
  stated contract and a test that enforces it.
- `stackconf/staging.go`, `staging/staging.go` — already split correctly behind
  an interface owned by the service layer.
- `stackconf/serialize.go` — correct *as a mirror*; its only sin is being the
  second of three copies of one mapping (see the three-mirrors finding).
