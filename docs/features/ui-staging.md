# UI Staging — staged changes, applied as a transaction

Railway-style: structural edits in the UI don't hit Docker immediately. They
pile into a per-environment **pending set**, shown as a diff with a hover box
("3 pending changes on production — review & apply"). Applying runs them
atomically through the existing reconcile engine; discarding drops them.

## Decisions (locked)

- **Only mutations stage; every read is instant.** Viewing logs, metrics,
  status, deployments, backups, the canvas — never staged, never gated.
  Staging applies solely to changes (create / edit / delete). Deletes stage
  too and stay gated as today.
- **Approach A1 — reuse the plan/apply engine via a config round-trip.** UI
  edits produce a *desired config*; the existing differ makes the plan;
  `execute` applies it atomically. One reconcile engine for both git and UI.
- **Auto policy governs git only.** With staging, the UI path is *always*
  manual-review — you stage, then apply. An env's auto/manual policy keeps its
  meaning only for git pushes.
- **Staged state is always shown as if it exists.** A staged-but-unapplied
  tile renders on the canvas (with a pending marker), not just in the review
  list.
- **Scope: per environment.** Matches the existing env-scoped plans, policy
  (auto/manual), protected-env gating, and always-gated deletes.
- **Shared pending set, with author.** Everyone editing an env adds to one
  pile; each staged change is stamped with who made it (shown in the diff and
  the hover box). Not per-user isolation.
- **Staged (structural):** create/delete tile, env vars, settings (source,
  port, limits, headers), domains, volume attach/detach, shared-db
  provision/attach/detach, scope, watch-paths.
- **Immediate (operational):** stop/start, restart, rollback, logs,
  run-cron-now, run-job-now, backup-now — act on what's already live.
- **Code/commit deploys stay immediate.** A git push or "redeploy latest
  commit" deploys that service now. Only *structure* is staged.
- **UI-managed stacks always; config-managed stacks opt in.** A config-managed
  stack keeps git as the source of truth by default and the UI stays read-only
  there. Declaring `ui_edits: stage` (stack file, or an org `defaults:` block)
  points its edits at the same pending set instead: the panel wins until the
  next config plan, which shows the change as drift and overwrites it on apply,
  terraform style. Nothing is written back to git.
  `editGate` in `handlers/web/handler/app/handler.go` is the single decision
  point. It does not cover managed-database tiles or domain resources — the
  buffer is keyed by tile slug and neither is a tile — so those still block.

## Representation

The differ takes `Resolved` (desired config) vs `State` (current DB tiles);
the applier reads desired values from `Resolved`. So staging must yield a
`Resolved`. Two ways considered:

- **A1 (recommended) — config round-trip.** Serialize the env's committed
  tiles into a `Resolved` (the read-side mirror of the existing
  `applyTileConf`), overlay the staged edits, and feed that to `Diff` +
  `execute`. Maximum reuse; unifies UI-managed with config-managed. Cost: write
  the serializer + let the applier take an injected `Resolved` instead of
  loading from git.
- **A2 — draft tiles.** Shadow tile rows edited by the UI, a new tile-vs-tile
  differ, and a tile-driven apply. No serializer, but duplicates the differ +
  applier logic that A1 reuses (deletes, domains, volumes, db provisioning are
  already handled and round-trip-tested). Rejected as more total code.

**Chosen: A1.**

### Staged edits = a per-env patch log

`staged_changes` table:

| col | meaning |
|---|---|
| id | pk |
| env_id | the environment |
| author_id | user who staged it |
| tile_slug | target tile ("" = env-level, e.g. create/delete tile) |
| op | set-field \| create-tile \| delete-tile |
| field | which field (for set-field) |
| value | new value (JSON/string) |
| created_at | for ordering + display |

Staged `Resolved` = `serialize(committed tiles)` + apply(patch log in order).
Last write to a field wins (shared pile). Individual entries are discardable;
"Discard all" clears the env's log. The pending diff is computed live —
`Diff(stagedResolved, Snapshot(env))` — so no plan is persisted until apply.

## Apply flow (reuses existing engine)

1. Build `stagedResolved` (serialize + patches).
2. `plan = Diff(stagedResolved, Snapshot)` — same plan object the git flow uses.
3. Gate: reuse `PolicyFor` + always-gated deletes + protected-env rules. On a
   manual/protected env, apply needs approval (existing approve/reject UI).
4. `execute(ctx, stack, stagedResolved, plan, opts)` — the already-separable
   reconcile core (bypasses `ApplyPlan`'s git load).
5. On success: record a `config_plan` row for history/audit, clear the env's
   staged log.

Delete gating, domain sync, volume/db handling, secret publishing all come for
free — they're already in `execute`.

## Handler changes

Every **structural** UI handler stops calling `engine.Enqueue`/`Deploy`
directly and instead appends a `staged_changes` row (with the current user as
author) and returns the updated pending count. Operational handlers and
code-deploy paths are unchanged.

Affected (append-to-staging instead of apply-now): tile create/delete, SaveEnv
(structural part), SaveSettings, domains CRUD, volume attach, provision /
attach / detach, db scope. ~8 handlers.

## Canvas / UX

- Tiles render their **staged** state with a pending marker (new = ghost/dashed
  "will be created", deleted = struck/dimmed, changed = a dot). This is what
  Railway does — you see the future state, not the live state, while staging.
- **Hover box** (bottom-right of the env canvas): "N pending changes · review &
  apply". Opens a diff panel (reuse the existing plan-view component) listing
  each change with its author + a per-row discard, plus Apply and Discard-all.
- Apply respects env policy: auto → applies; manual/protected → creates a plan
  that waits for approval (existing flow).

## Concurrency

Shared pile, last-write-wins per field. Two people staging different tiles →
independent entries. Same field → later entry wins, author updates. No locking
in v1 (self-hosted, small team). This is the current scaling ceiling.

## Phases

1. **Serializer + apply core. ✅ DONE.** `StateToResolved` / `tileToConf` in
   `internal/stackrd/config/stackconf/serialize.go` (read-side mirror of `applyTileConf`), and
   `Applier.ApplyResolved` — the injectable entry that runs the same gate +
   `execute` from a caller-provided `Resolved` instead of loading from git.
   Proven by `TestStateToResolvedRoundTrip`: a representative env (git service
   with domains/env/limits/volumes/watch-paths, image service, cron, db,
   attached volume) round-trips to an empty diff; `…DetectsEdit` confirms a
   staged edit still shows.

   **Phase-2 opening decision (resolve FIRST, before the staging store):**
   `TileConf` cannot hold a git tile's *source identity* — there is no
   `git_url` / `connector` field. Today those come from the bound
   repo via `opts.GitURL` at apply time. UI-managed stacks have no bound repo,
   so staging the *creation* of a git service would write `GitURL=""`. Editing
   an existing git service is fine (applyTileConf leaves URL untouched); only
   creation is affected. Decide: either grow `TileConf` (+ `applyTileConf` /
   `diffTile`) with git-source fields, or keep git-service **creation**
   immediate while everything else stages. This determines whether A1's config
   model can represent a UI tile at all.

   **Phase-2 first acceptance gate (before any hover-box/review UI):** a hard
   assertion against a *real* env, not fixtures — opening a just-committed env
   shows **0 pending changes on untouched tiles**. If it doesn't, the
   serializer missed a real-data field. (Phase-1 proved this on representative
   fixtures only; the live assertion is deferred to here, deliberately.)

   **Other field-coverage gaps** (`diffTile`/`applyTileConf` don't compare
   these, so they can't stage through the config engine): published_ports,
   basic auth, traefik_override. Stage the
   covered fields; keep these immediate with a note, or extend the differ.
   Also: multi-line env values are a known round-trip edge (canonEnv
   lossiness) — fix in canonEnv.
2. **Staging store + one staged action. ✅ DONE.** `staged_changes` table
   (now in `001_initial` after the migration squash) + repo CRUD; `Applier.StagedResolved`/`StagedPlan`/
   `ApplyStaged` build the desired config from committed tiles + patches and
   reconcile via the existing engine. `SaveEnv` on **UI-managed** stacks stages
   the env edit (per-tile, author-stamped) instead of deploying; config-managed
   stacks unchanged. Canvas shows a bottom-right "N pending change(s) — review &
   apply" box; the review page lists staged edits (tile · summary · author ·
   discard) + the live diff, with Apply / Discard-all. Verified live on the
   UI-managed Demo stack: edit env → stages (no deploy) → box appears → review
   shows `~ web env → STAGED_TEST=hello` by author → Apply reconciles (tile env
   committed + container redeployed with the new var) → staged cleared; the
   reverse (removing the var) works the same.

   **Grew `TileConf` with git-source fields first** (`git_url`, `connector`,
   `connector`) + diff/apply/serializer coverage, so a UI git tile round-trips
   through create too — resolving the phase-2 opening decision.

   Deferred to phase 3: the acceptance gate (live 0-diff on untouched tiles —
   effectively proven by the Demo round-trip above), routing the other
   structural handlers, canvas pending markers on tiles, and build_args (saved
   immediately for now — it isn't in the config model).
3. **Route the rest of the structural handlers + canvas markers. ✅ DONE.**
   - **Generalized payload**: the staged blob is now `{op, patch}` — `op` is
     `update|create|delete` (in the blob, no new column). `StagedResolved`
     patches on update, injects a full `TileConf` on create, tombstones on
     delete (a delete supersedes an update on the same tile regardless of
     order). `mergePatch` uses JSON replace-semantics, nulling `Env` first so a
     removed var actually disappears. Sparse patches are raw maps (env, domains,
     settings) so cleared fields apply and untouched groups aren't clobbered;
     one row per (tile, summary-group) → per-group last-write-wins.
   - **Field coverage** (settings form): added `build_args`, `published_ports`,
     `traefik_override`, `basic_auth_user/hash`, and cron
     `allow_overlap` to `TileConf`/`diffTile`/`applyTileConf`/`tileToConf`. json
     tags mirror yaml so patches merge under the same names. `TestApplyStagedPatch`
     covers the three ops; the round-trip test carries the new fields.
   - **Handlers routed** (UI-managed stacks): `SaveEnv` (env + build_args now in
     one staged group — no more direct build_args write), `SaveSettings`,
     `Attach` (volume), `Delete` (app tile, op=delete), `CreateTile` +
     `CreateDB` (op=create), and **domains** — `CreateDomain` /
     `DeleteDomain` / `ToggleDomainHTTPS` stage the tile's whole desired domain
     set (a "domains" group). `currentDesiredDomains` builds each edit on the
     pending staged set (else a second add clobbers the first). Shared
     `staging.Stage` helper stamps the author and dedupes per group.
   - **`deleteTile` hardened**: now compose-downs and orphans shared-db
     provisions (matching the `Delete` handler) so staged deletes and
     config-managed git deletes tear down identically.
   - **Canvas markers**: `graph.Node.Staged` ("pending"/"delete"), set post-build
     from `staged_changes`; the card shows an "edited"/"removing" corner badge +
     ring. Staged *creates* have no committed node → shown in the pending box +
     review page only (rowless ghost tile deferred).

   **Carve-outs (kept immediate, by design):**
   - **Shared-db provision/attach/detach** — `TileConf` has no provisions
     concept and provisioning creates a real logical DB in a live container
     (imperative infra, not declarative config). The config-shaped part (the
     reference injection) already rides through env, which stages.
   - **Domain custom cert** (`SetDomainCert`) stays immediate — TLS certs aren't
     (and shouldn't be) in declarative config. **Per-domain container ports**
     aren't in the config model either (it uses the tile's port), so a custom
     per-domain port applies only on the immediate path. You can only stage
     delete/toggle of *committed* domains — a staged-but-unapplied new domain has
     no row/button; discard it from the review page.
   - **db package** (external port, sharing scope, drop-logical-db) and **env
     secrets** — not modeled in the config engine.
   - **compose-source tiles** — the compose deploy path isn't modeled in the
     config engine; SaveSettings/CreateTile stay immediate for them.

   **Verified live** (local `hamr dev`, UI-managed "My Shop" stack): settings
   edit (port + `published_ports`) → staged in 2 ms, no deploy → "edited" badge
   + pending box → review shows the diff by author → apply commits (8081 +
   `8443:8443`) and redeploys; staged **create** (image service) → rowless,
   shown in box/review as `+ create` → apply builds + deploys it; staged
   **delete** → tile stays with a "removing" badge → destructive apply confirm
   → torn down; staged **domain** add (`+api.staged-test.dev`) and remove
   (`-api.staged-test.dev`) → apply commits/removes the row. No server errors
   across the run.

   **Known warts (v1):** after staging a settings/env edit the panel shows the
   edited values on the htmx response but a full page reload re-reads the
   committed tile (the review page is authoritative); saving settings with no
   change still stages a no-op row (review shows "no effective change"); a
   staged *create* is named by its slug (`cache-svc`), losing the typed display
   name — `TileConf` keys tiles by slug and has no name field.
4. **Polish:** per-row discard (done), author display (done), plan history
   record, protected env approval wiring, canvas ghost for staged creates.

## Resolved

- Auto policy governs git pushes only; the UI staging path is always
  manual-review. (See Decisions.)
- Staged-but-unapplied changes always render on the canvas as pending. (See
  Decisions.)
- Reads never stage — only create/edit/delete. (See Decisions.)
