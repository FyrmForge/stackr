# Env tiers and per-env params: design (in discussion)

Status: decided 2026-10-09; built per env-tiers-build.md.

## Today
- Each stack names its own envs (its ladder); nothing ties one stack's prod
  to another's.
- Org params hold one value for the whole org, read explicitly with
  `${{ org.params.c.n }}`.
- The stack file's top-level `params:` writes one value into every env; a
  hand-set env value is put back on the next promote. Per-env values today:
  declare without a value, set each env by hand.

## Decided
1. Tiers. The org defines an ordered list of tiers (dev, staging, prod).
   A stack env named like a tier joins it and gets the tier features. A
   stack may define other envs; those are off-tier and lose tier features.
   PR envs (`pr-N`) are off-tier.
2. Off-tier envs read no tiered value implicitly.
3. Explicit tier refs: `${{ org.params.smtp[prod].host }}` reads another
   tier's value, params and secrets alike, subject to the lock (4).
4. Locks. A locked env's values are read only from inside it; an explicit
   `[env]` ref from elsewhere is refused. The lock lives where the env is
   defined: org file for org tiers, stack file for a stack's own envs.
   Every tier starts locked.
5. Who changes a lock: someone with write at that level (org owner for an
   org tier, member or above for a stack's own env).
6. A lock change in a file never applies by itself: it is a plan step that
   waits for approval by someone with write at that level; the rest of the
   plan applies as usual.

7. File shape. Tiers and their locks are defined in the org file; a stack's
   own envs and their locks in the stack file. Params live in groups, and
   each group holds one block per env (tier or the stack's own env). A plain
   value is written as is; a secret is `{ type: secret }` (optionally
   `generate: N`), never a value in git. A name an env block does not set is
   unset in that env. No "every env" shortcut yet.

   ```yaml
   tiers:                       # org file, bottom first
     dev:     { locked: false }
     staging: { locked: true }
     prod:    { locked: true }
   params:
     smtp:
       dev:
         host: smtp.dev.example.com
         password: { type: secret }
       prod:
         host: smtp.example.com
         password: { type: secret, generate: 32 }
   ```

8. Refs. `${{ params.c.n }}` reads the stack's group, this env's block.
   `${{ org.params.c.n }}` reads the org's group, this env's tier block
   (off-tier: unset, the tile waits). `${{ org.params.c[env].n }}` (and
   `params.c[env].n`) reads another env's block, refused when it is locked.
   A stack param is no longer one value for every env.
9. PR envs read only a group's `pr` block (`pr` is a reserved env name),
   never another env's values; no `pr` block = nothing, the tiles wait.
   Explicit `[env]` refs work from a PR env when that env is unlocked.
10. One block may name several envs: `dev|staging|pr:`. Plain values are
    shared; a secret declared there is still its own value per env. Two
    blocks setting the same name for one env is refused at read, naming
    both blocks.

11. Config is like Terraform: the UI can edit anything, file-defined or not.
    A UI edit that differs from the file is drift; the next plan shows it and
    applying puts the file's value back. Secrets have no value in the file,
    so a UI-set secret is never drift.
12. Everything in this design is doable in the UI too (tiers, locks, param
    groups, env blocks, secrets). A new org has no tiers until it adds some.

## Parked
- Stacks talking to each other along tiers ("prod only reaches prod").

## Open
- What the panel shows and edits.
- Migration: none (no users); existing stacks re-declare.
