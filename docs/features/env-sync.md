# Feature: Environment compare and sync

Status: planned, not built. Agreed with darthvader 2026-10-02; design pass
2026-10-05.

## Summary
Railway's sync, copied. On a stack managed in the UI (no config repo), the env
that should receive changes has a Sync button. The user picks the env to sync
from, each tile card that would change is tagged New, Edited or Removed, the
user reviews the changes, then deploys them.

## The rules
- **Only stacks managed in the UI** get sync.
  A config-as-code stack never does: the file in git is the truth, and stackr
  does not push commits.
- **Setup only.** Synced: tiles added or removed, tile settings, variables,
  secrets (by name), volumes. Never synced: versions (which image or commit
  an env runs) and domains (each env has its own).
- **Ladder envs** (`from: promote`): promote keeps moving versions up the
  ladder, as today. Sync is only for setup.
- **Branch envs** (`from: <branch>`): no promote. Each env builds its own
  branch, so versions always differ and are ignored. Sync is the way to carry
  setup across.
- **Nothing is copied on promote.** A tile that exists only in dev (a Stripe
  mock, a mail catcher) stays only in dev until someone syncs it on purpose.

## Why
- Copying all of dev up the ladder would push dev-only tools into staging and
  production. Only a config file can say "dev gets this, production doesn't".
- Config-as-code already handles per-env setup: a base set of tiles plus
  overrides per env, each env computed from the file.

## User Stories
- As an org owner on a UI-managed stack, I want to see that staging lacks
  the worker I added in dev, so production does not ship without it.
- As an org owner, I want to copy one tile or one setting across envs,
  so I do not redo it by hand.
- As an org owner, I want dev-only tiles to stay in dev unless I sync them.

## Acceptance Criteria
- [ ] The env canvas of a UI-managed stack has a Sync button.
- [ ] No Sync on a stack bound to a config repo.
- [ ] After picking a source env, each changed tile card is tagged New,
      Edited or Removed; nothing is applied yet.
- [ ] Versions and domains never show as changes.
- [ ] Deploy applies the reviewed changes; cancel drops them.
- [ ] Promote behaviour is unchanged.

## Decided
- Compare against any env of the stack the user picks; the default is the
  env below on the ladder (dev for staging). A branch env defaults to the
  stack's first env.
- Secrets sync by name only; the value is set per env.
- Railway's flow: Sync button, pick the source env, cards tagged
  New / Edited / Removed, review, deploy.
- Domains are never synced or shown.
- Removed: a tile missing from the source env is deleted, as Railway does.
  Its volume is kept (orphaned), as promote does; the review notes it.
- A tile's `image` and `branch` are never synced (they are the version);
  `git_url`, dockerfile and build args are.
- Managed tiles: a New one is provisioned fresh and empty in the target,
  never with data; its allow list and env pairs sync. A slice's
  `provision_from` is copied as is; a target the resolver cannot reach
  blocks the deploy, as in promote (`planSlice`).
- A secret synced by name arrives unset; the review warns which tiles read
  it, as promote's `planParams` does.
- Removing a managed tile whose instance still serves slices is blocked,
  with promote's check (`planDeletes`).
- Permission: `env.write`, as promote.
- A cron tile's `paused` is never synced; the target keeps its own, as
  promote does.
- A sync makes no release: the job redeploys the changed tiles, as a tile
  edit does (`UpdateTile`, internal/service/tile.go). Rollback cannot undo it.
- Review: the user can drop single tiles (not single fields) before deploy;
  Deploy posts the kept tile slugs.
- No passive "differs" hint on the canvas; the Sync button only.
- CLI: `stackr env sync --from <env> [--tile a,b] [--dry-run]` at `--env`,
  shaped like promote (`move` in cmd/stackr/stack.go): plan, refuse on
  blockers, confirm, follow the job. `--dry-run` is the diff; no `env diff`.
- Nothing is stored: the review is `?sync=<env>` on the env page, recomputed
  from live rows; the job re-plans on deploy and refuses on drift.

## Technical Notes
- Today's promote (`internal/service/internal/flow/promote/plan.go`): a
  release of a stack with no config repo carries image pins only, and a
  promote moves them into the tiles the target env has. That stays.
- The diff is computed on demand from live rows (tiles, params, volumes);
  no new table. API pairs like promote: GET `env/sync-plan/:from`,
  POST `env/sync/:from` with the kept slugs.
- Sync is a job through the orchestrator's existing verbs (tile create and
  set, params set), like the org config apply.

## UI/UX
- Sync button at the top of the env canvas, next to the env name.
- Changed cards carry a New / Edited / Removed tag; a bar across the canvas
  says "3 changes from dev · Review · Deploy · Cancel".
- Review is a drawer: per tile, what changes, old and new side by side, and
  a drop button each. Unset secrets and orphaned volumes show as warnings.

## Testing
- Unit: the diff ignores versions, and finds a missing tile, a changed
  setting and a missing variable.
- Unit: a config-bound stack yields no diff.
- E2E on the rig: add a tile in dev, sync staging from dev, see it tagged
  New, deploy it.
