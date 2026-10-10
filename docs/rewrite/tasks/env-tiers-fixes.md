# Env tiers: review fixes

Status: built 2026-10-10, on the rig as v0.6.0-dev.32, QA passed. Source: Codex (`codex review --uncommitted`)
plus four Opus reviewers (security, correctness, files/plans, web/API/CLI)
over the uncommitted env tiers work (dev.31). Static: gitleaks (only local
dev files, generated code, vendor js), govulncheck (two known docker
advisories, no fix, not from this change).

## Fix now (clear)
1. Org-file lock changes auto-apply (HIGH). `orgconfig/diff.go` `tier-lock`,
   `tier` with `locked: false`, and `tier-rename` rows carry no Impact, so
   `AutoOK` passes and an auto org plan applies them with no owner. Give
   unlocks, unlocked new tiers and renames an Impact line: never auto.
2. Lock and tier changes never redeploy (HIGH). `SetTierLock`, `SetEnvLock`,
   `CreateTier` (first tier), `DeleteTier` (last tier), `RenameTier`,
   env rename to/from a tier slug, and the org-file apply of those rows only
   write rows: forbidden values keep running. After each, redeploy the
   readers of the affected scopes (all keys); a rename matches old and new
   slug.
3. Template folders survive a lock (HIGH). `mount_files.go paramsVersion`
   stamps counts and `updated_at` only; fold every tier's and static env's
   `slug:locked` and the env's own tier slug into the key.
4. Secrets in `provision_from` reach plan text and job logs (MEDIUM). Refuse
   secrets for `InProvisionFrom` as for `InDomain`.
5. Removing `tiers:` from the org file does nothing (MEDIUM). Diff an empty
   or absent `tiers:` against live tiers as removal rows; note that org-wide
   params are read again once the last tier goes.
6. Tier rename moves envs out silently (MEDIUM). `movedTiers` and
   `RenameTier`: a blocker (file) / conflict (verb) while stack envs carry
   the old slug, as delete does.
7. `pr` is not reserved as an env name (all reviewers). Refuse it in
   `environment.name()` and the stack file's env names.
8. Grid secret kind per cell (Codex, web). `vars.go`: track secret per cell;
   a plain cell never renders masked or posts as a secret.
9. Tiered env drawer always shows Locked (web). Read the tier's lock.
10. Org-scope writes in a tiered org are stored and never read. Refuse
    `org`-scope writes once the org has tiers ("this org has tiers; use
    --tier or --pr"); `reach` for `org` excludes PR envs and tiered orgs.
11. Share login `[x]` refs (Codex, correctness): resolve through the full
    `ParamSnapshot` (locks apply); `params.*` stays refused.
12. Promote and sync write params with a direct `Merge`: route through
    `changeParams` so `[x]` readers in other envs redeploy.
13. Small: tier up/down arrows reversed on screen; reserve tier slug
    `order`; `--tier`/`--pr` only on params commands and conflicting flags
    refused, `--pr` with no stack refused unless `--level org`; Unset gets a
    confirm; secret inputs are password type in the Add form; accessible
    names on lock checkboxes and Unset buttons; `SetEnvLock` refuses a PR
    env; sync into a PR env refused; template tiles restart only when the
    changed block is readable from their env; `params import` op list.

## Decided
- D1 Each PR env keeps its own params (its `env` scope), like before tiers.
  On creation it is seeded with a copy of the stack's shared `pr` block
  (`stack_pr`), secrets included. Each push writes that branch's plain `pr`
  values into its own copy only; a branch never changes secret values, only
  declares names. The copy is deleted with the env. `stack_pr` is the
  template, written only from the panel or a non-PR promote (main's file);
  a secret changed there is pushed into open PR envs' copies too. The org
  `pr` block (`org_pr`) stays shared: only the org file or panel writes it.
- D2 Terraform way: a name a file group no longer sets for an env (a name,
  an env block, or the whole group dropped) is a plan row "remove <group>.
  <name> from <env>"; applying deletes it. Plain values go with the plan
  (auto envs too); a secret needs a tick (removal row) since a generated
  one cannot be recovered. Everything in a file-bound scope is managed (as
  `planVolumes` treats volumes): a name the file does not set is removed,
  panel-made ones too. Same for org `tier`, `org_pr` and `org` scopes when
  an org file is bound.

- D3 Joining a locked tier (create or rename an env to its slug) needs
  `tier.write` (org owner): one service check used by web, API and CLI.
  The stack file's `ladderEnvs` skips such an env with a "not made" log
  line ("env prod: joins locked tier prod; an org owner makes it in the
  panel"). Unlocked tiers stay open.
- D4 One org grammar: untiered orgs use the tiered shape with env keys
  `all` (the `org` scope) and `pr` (`org_pr`); `Params.Wide`/`CheckWide`
  go. The grid shows columns `all` and `pr` for an untiered org.
- Also: any `CreateTier`/`DeleteTier` whose slug matches live envs
  redeploys those envs' tiles (not only first/last tier).

Decided with a Fable second opinion (Terraform default), per darthvader.
