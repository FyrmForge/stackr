# Env tiers and per-env params: build plan

Status: built 2026-10-09, on the rig as v0.6.0-dev.31, QA passed (no PR env or config-repo paths: those need GitHub). Design and decisions: `env-tiers.md` (1-12).
No users: no data migration, existing params rows of the dropped scopes are
deleted by the migration; stacks re-declare.

## Model (what every lane builds against)

Tiers
- `tiers` table: `id, org_id, slug, position, locked, created_at`,
  `UNIQUE (org_id, slug)`. Ordered by `position` (bottom first).
- An env's tier: the org tier whose slug equals the env slug. No column.
- A PR env (`environments.type = 'ephemeral'`) is never in a tier, whatever
  its slug.
- `environments.locked` (bool, default true): the lock of a stack's own
  (off-tier) env. A tiered env uses its tier's `locked`; its own column is
  ignored.

Param scopes (`params.scope_kind`)
| kind       | scope_id | holds                                        |
|------------|----------|----------------------------------------------|
| `env`      | env id   | the stack groups' block for that env (kept)   |
| `tier`     | tier id  | the org groups' block for that tier (new)     |
| `stack_pr` | stack id | the stack groups' `pr` block (new)            |
| `org_pr`   | org id   | the org groups' `pr` block (new)              |
| `org`      | org id   | org-wide values for an org with no tiers (kept, see Open) |
| `server`   | -        | unchanged                                     |
`stack` is dropped: a stack param is no longer one value for every env.

Reads (the consumer env E, in stack S, org O)
- `${{ params.c.n }}`: E is a PR env: `stack_pr` of S. Else `env` of E.
- `${{ org.params.c.n }}`: E is a PR env: `org_pr` of O. E in tier T:
  `tier` T. E off-tier: unset (the job parks). O has no tiers: `org` of O.
- `${{ params.c[x].n }}`: `env` of S's env x (x = `pr`: `stack_pr`).
  Refused when x is locked and x != E.
- `${{ org.params.c[x].n }}`: `tier` x (x = `pr`: `org_pr`). Refused when
  tier x is locked and E is not in x.
- `pr` as an `[x]`: PR blocks are never locked (they hold sandbox values
  by design).
- Every refusal names the ref and the lock: "`${{ org.params.db[prod].pass }}`:
  prod is locked; unlock it in the org's tiers to read it from here".

## Lanes

Contract first: lane A lands the types and service verbs below before B, C
and D start on anything that calls them. Each lane runs `make test` and
`make lint` before it reports. Never touch `*_templ.go` or `frontend/`.

### A. Data, resolver, deploy (one worker)
1. Migration `010_env_tiers`: `tiers` table; `environments.locked`;
   rebuild `params` with the new `scope_kind` CHECK; delete `stack` rows.
   Down migration restores 009's shape.
2. `store`: `TierStore` (List by org, Get, Create, Update, Delete,
   Reorder); `Environment.Locked`.
3. New leaf `leaf/tier`: CRUD, slug rules (`slug.Valid`, `pr` reserved),
   `Of(org, envSlug) (Tier, bool)`.
4. `leaf/params/ref.go`:
   - `Ref.Env` (the `[x]` qualifier); `Parse` takes `c[x]` in the
     collection position for `params` and `org.params` only.
   - `Snapshot`: drop `StackParams`; add `Other map[string]Block` keyed
     `"stack:<env>"` / `"org:<tier>"` (and `:pr`), `Block{Locked bool,
     Values map[string]Value}`; add `Self string` (E's slug) and
     `Tier string` (E's tier slug, "" off-tier).
   - `lookup` per Reads above. The resolver still makes no store call.
5. `flow/deploy/facts.go snapshot`: fill the snapshot per Reads (load
   every tier block and every env block of S: small; locked ones get
   `Locked` set and no values). `mount_share.go`, `proxy.go`, `slice.go`,
   `mount_files.go` (params version), `graph.go`, `promote/plan.go:1198`
   read through the same builder, never their own scope.
6. Service verbs (`internal/service/tiers.go`):
   `Tiers(ctx, orgID)`, `CreateTier(ctx, orgID, slug)`,
   `RenameTier`, `ReorderTiers(ctx, orgID, []slug)`, `DeleteTier`,
   `SetTierLock(ctx, orgID, slug, locked)` (owner);
   `SetEnvLock(ctx, envID, locked)` (member; refused on a tiered env:
   "prod's lock is the org's tier lock").
   `ParamScope` gains the new kinds; `Params`/`SetParams`/`DeleteParam`
   accept them.
7. `params.go readers` (the redeploy filter): a change in scope K reaches
   the tiles that read it per Reads: `tier` T -> tiles of every env of O in
   T reading `org.params`, plus any tile with `org.params.c[T].n`;
   `org_pr`/`stack_pr` -> PR env tiles; `env` x -> x's tiles plus any tile
   of S with `params.c[x].n`. `scopeTiles` grows the same cases.
8. Tests: resolver table test (every row of Reads, each lock refusal, PR
   env, off-tier parks); migration up/down; readers per scope kind.

### B. File grammar (one worker, after A.4 types)
1. `planfile`: a group is `map[envKey]map[name]Entry`; `Entry` is a plain
   string value or `{type: secret, generate?: N}` (custom YAML unmarshal).
   `envKey` is `slug` or `a|b|c`; split on `|`; each part `slug.Valid`.
   Two blocks setting one name for one env: error naming both keys.
   `CheckParams` and `IsRef` move along.
2. Stack file (`promote/stackfile.go`): `params:` in the new shape (env keys
   are the stack's env slugs or `pr`; an unknown env key is an error);
   `environments.<env>.locked`; includes merge groups per env block.
3. Org file (`orgconfig/file.go`): `tiers:` (ordered map: slug ->
   `{locked}`); `params:` in the new shape (env keys are tier slugs or
   `pr`; an unknown tier is an error).
4. Tests: grammar table (pipes, conflicts, secrets, unknown keys, reserved
   `pr`), round trip YAML.

### C. Plans, apply, export (one worker, after A.6 and B)
1. Stack plan (`promote/plan.go planParams`): per env, diff the file's
   block for that env against `env` scope (PR envs against `stack_pr`):
   add, change, becomes-secret, generate, drift (UI value differs: the
   plan shows "panel has X, file says Y" and applying sets the file's).
   Secrets never drift.
2. Stack env lock: a file lock that differs from live is a plan row
   "demo: file says unlocked, live is locked" that does NOT apply; the
   stack drawer's plan row has an "Apply lock change" button (member).
3. `sync.go syncParams`: copying an env copies its `env` block only.
4. Org plan (`orgconfig.go`): tiers create/rename/reorder/delete/lock rows;
   tier blocks and the `pr` block as param rows against `tier`/`org_pr`.
   Org plans already need an owner's approval, so lock rows apply with it.
   Deleting a tier still named by a stack env is a blocker.
5. Exports (`promote/export.go`, `orgconfig/export.go`): write the new
   shape; merge identical blocks into `a|b` keys.
6. Tests: plan rows per case, drift, lock row not applied, export round trip
   (export, re-plan: no changes).

### D. API, CLI, UI (one worker, after A.6)
1. API (`api/routes.go`, `handler/v1`): `/orgs/:org/tiers` (list, create,
   rename, reorder, delete, lock); env lock under the env route; params
   routes for `/orgs/:org/tiers/:tier/params`, `/orgs/:org/pr/params`,
   `/orgs/:org/stacks/:stack/pr/params`; stack-level `/params` routes go.
   `docs/openapi.json` regenerated.
2. CLI (`cmd/stackr`): `stackr tier ls|add|rename|order|rm|lock|unlock`;
   `stackr env lock|unlock`; `stackr params ... --tier <t>` and `--pr`;
   `--level stack` goes.
3. UI: org drawer Tiers section (add, rename, reorder, delete, lock
   toggle; owners only). Params editor (`canvas/vars.go` and its templ):
   one grid per group, a row per name, a column per env (stack: its envs
   and `pr`; org: its tiers and `pr`), secrets masked; editing a cell sets
   that scope. Env drawer: lock toggle on off-tier envs. A design board on
   fraedi first (tiers section, the grid), approved by darthvader.
4. Tests: route gating (`TestEveryRouteGated`), parity test, web handler
   tests for the grid and tiers section.

## QA on the rig
Org with tiers dev and prod (prod locked); stack with dev, prod, a `demo`
env and PR envs on. Check: each env reads its block; demo reads nothing
from org; `[dev]` from demo works, `[prod]` is refused; a PR env reads
`pr` blocks only; a UI edit shows as drift on the next plan; a stack file
lock change waits for the button; an org file lock change applies on
approval.

## Settled after the design
- An org with no tiers keeps today's single org-wide values (`org` scope,
  and the org file's `params:` as today's shape). Adding the first tier
  switches to tier blocks; the plan that adds it warns that org-wide values
  stop being read.
- A tiered org has no single-value params: a value for several tiers is a
  `dev|staging|prod` key. An `all:` key can come later without breaking a
  file.
- Stack-level single values are gone (decision 8); PR blocks are never
  locked; a stack's own envs start locked.

## Built: cleanup lane (stack scope removed)
- Migration 010: `'stack'` out of the params CHECK, stack param rows deleted; the
  stack cascade trigger no longer names it. Tests updated.
- `leaf/params/ref.go`: `Snapshot.StackParams` and the env-to-stack fallback gone.
- Share logins (`deploy/mount_share.go`): a `${{ params.* }}` ref is an error
  naming the share; only `org.params` resolves.
- API: stack `/params` routes and the `stack` row of `scoped()` gone (stack
  volumes routes stay); `docs/openapi.json` regenerated.
- CLI: stack ops dropped from the params leaves, `--level stack` refused for
  params (kept for volumes); a linked stack with no `--env` says to pick
  `--env`, `--pr` or `--level org`.
- Canvas: the stack canvas no longer draws a vars card.
