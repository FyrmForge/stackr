# Plan: pre-cut freeze

Status: discussion, opened 2026-09-17. Nothing agreed, nothing built.

Working rules: discuss first, one point at a time, no code without a go, no
git writes, no edits to `*_templ.go` or `output.css`, terse UI copy.

## Why

`AGENTS.md` lets us break schemas freely because every install is a
disposable rig. The first install that holds someone's data ends that. This
plan is only the changes that are cheap now and expensive or impossible once
that install exists. Everything else waits.

Already shut, not in scope: migrations are one `001_initial` baseline
(squashed 2026-09-14), the API is `/api/v1`, config is `version: 1`, Docker
names are `stkr-` prefixed and volume names are stable.

## Items

### 1. The panel's own store: SQLite or PostgreSQL — decided 2026-09-17

**Stay on SQLite.** Revisit only if concurrent writers or provisioning app
databases off the panel's own instance become real needs, and pay for it then
with a migration script rather than now with a rewrite.

Why it was cheap to defer: the cost is not runtime (a socket hop instead of
an in-process call). It is the bootstrap chicken-and-egg (the installer would
have to stand up a database before the panel boots, and the panel cannot
cleanly manage the store it runs on), the backup and self-upgrade rewrite
(`scripts/restore.sh` untars `stackr.db` + `keys/master.key` today,
pg_dump/pg_restore against a live version-matched service otherwise), and a
container per test run for the 28 test files that touch the store.

The dialect sweep itself is mechanical and stays that way: 31 files, ~3.3k
lines, ~180 `?` placeholder lines, 11 `ON CONFLICT` sites, a 548-line
baseline, and no `json_extract` or `strftime` anywhere. That is what makes a
later migration script a fair trade instead of a trap.

### 2. The untested managed engines — already removed, docs are stale

The code half is done. `managedtiles.Engines`
(`internal/stackrd/infra/managedtiles/managedtiles.go:125`) holds `postgres`
and `s3` only; mysql went with them. There is no `engine_mariadb.go` and no
mariadb, mongo or redis anywhere in the tree. `docs/notes.md` has not caught
up.

What is left is text that still promises engines we do not ship:

- `docs/notes.md` — drop the removal item.
- `docs/features/backups.md:33,42,43,161` — dump and restore commands for
  mariadb and mongo, listed as shipping.
- `docs/features/shared-infra.md:3` — names mariadb and redis.
- `internal/stackrd/store/db/migrations/001_initial.up.sql:142,305` — column
  comments listing the dead engines.
- `internal/stackrd/config/stackconf/stackconf.go:649` — reads "no longer
  supported"; should read "not supported". `drift_test.go:117` already
  asserts the string is absent from one path.

Agreed 2026-09-17, not yet done.

### 3. The on-disk contract — decided 2026-09-17

**Current names are final.** If any of them ever changes, it ships with a
migration script; none of them changes quietly.

The contract, verified in code:

| thing | value | written by |
|---|---|---|
| data dir | `/var/lib/stackr` | `internal/installer/flags.go:77`, default `./data` at `cmd/stackrd/main.go:79` |
| swarm service | `stackr` | `internal/installer/steps.go:130` |
| panel archive | `stackr.db`, `keys/master.key`, `VERSION` | `infra/backup`, checked member by member in `scripts/restore.sh` |

The `stackr_panel` mismatch is settled: the installer creates `stackr`, so
`docs/qa/onboarding.md:20` is simply wrong and should say
`docker service logs stackr`.

Remaining work: write the table into `docs/features/backups.md` as a stated
contract rather than leaving it as whatever the scripts happen to do, and fix
the QA doc line.

### 4. Freeze `001_initial`, then additive-only migrations

Two halves. The rule, and a test that enforces it.

**The rule.** Up to the cut, every schema change edits the baseline and the
rig gets wiped (`internal/stackrd/store/db/dumpschema`; the two traps stand:
do not reflow the dump, drop order is topological, not reverse). After the
cut, `002_*` onward, forward-only, the baseline is never edited again.

**When it flips: the last act of this plan.** Decided 2026-09-18. Items 1-3
land first, the doc cleanups with them, and the final change closes the
baseline and adds the guard in the same commit. Until that commit the
baseline is still fair game and the rig still gets wiped.

**The guard.** A migration after the cut may add, never destroy. A rename or
a drop against a database holding someone's data is the one mistake with no
undo, so the deny-list is a test, not a convention nobody reads.

The guard lands with the flip, not before: while the baseline is still open
every migration is `001_initial` and there is nothing for it to check.

Shape (agreed 2026-09-18, not built):

- A test beside `internal/stackrd/store/db/migrate_test.go`. It reads the
  embedded `migrations/*.sql` (`db.go:13`), skips `001_initial`, and fails on
  destructive statements in any `*.up.sql`.
- Deny: `DROP TABLE`, `DROP COLUMN`, `DROP INDEX` on a shared index,
  `RENAME TO`, `RENAME COLUMN`, `TRUNCATE`, `DELETE FROM`. Comments and
  string literals stripped before matching, so a word inside a comment does
  not trip it.
- `*.down.sql` is exempt. A down migration is drops by definition.
- Escape hatch: a `-- migration-guard: allow <reason>` line immediately above
  the statement. Deliberate destruction stays possible and stays visible in
  review; nothing gets through silently.
- A drop we actually want becomes two releases: stop writing the column,
  then remove it once no running version reads it.

## Explicitly not in this plan

Fixable after the cut without stranding anyone's data, so they wait:

- The panel attaching to instance overlays and taking the service VIP
  (`docs/notes.md`, seen on the rig, silent for every consumer).
- `files:` resolving on the manager only.
- No panel self-monitoring, silent stuck work queue.
- Per-logical-db backups, `public`-schema-only Postgres restore.
- The recovery CLI that cannot act as admin.
- The onboarding chain (`docs/qa/onboarding.md`) has never been run green;
  `docs/qa/rounds` does not exist. Not an item, but it gates whether items
  1-3 can be validated at all, since a frozen on-disk contract that no
  first-boot run has ever exercised is a frozen guess.

Those last three are the reasons a first real install is still a bad idea;
they are not reasons to change the shape of anything before it.

## Built, 2026-09-18

Another session ran items 2, 3 and 4 in one pass, after the onboarding chain
went green twice (`docs/qa/rounds/2026-09-17-onboarding.md`,
`2026-09-18-onboarding.md`), which is the gate the section above named.

- **Item 2.** Doc sweep, wider than the five bullets listed: `docs/notes.md`
  (item dropped), `docs/features/backups.md` (mode table, pinned commands,
  acceptance criteria, technical notes, testing), `docs/features/shared-infra.md`
  (`:3`, `:12`, `:18`, the roadmap row), `docs/features/config-as-code.md:48`
  (the `engine:` comment), `001_initial.up.sql:142,305`, and
  `infra/managedtiles/fork.go:8` ("postgres and mariadb pipe dump into").
  The fifth bullet is void: `stackconf.go:653` is `removedKeys`
  (`compose`, `compose_inline`, `compose_path`), where "no longer supported" is
  accurate, and the unknown-engine error the note meant is `stackconf.go:1091`,
  which already reads "is not supported, stackr manages postgres and s3".
  Left alone deliberately: the ADR, `plans/46`, `restorecmd_test.go`'s
  "(If mariadb comes back…)", and `redis:7` used as a plain service image.
- **Item 3.** The table is now a stated contract under "The on-disk contract"
  in `docs/features/backups.md`, re-verified against the code today
  (`installer/flags.go`, `installer/steps.go`, `infra/backup/backup.go:504-519`,
  `panelarchive_test.go`, `scripts/restore.sh:29`). The QA doc's service-name
  line was already fixed.
- **Item 4, the flip.** `001_initial` is frozen: header on the file itself, the
  rule in `AGENTS.md` (both the "break schemas freely" bullet and the Database
  section), and the comment on `db.Migrate`. The guard is
  `internal/stackrd/store/db/migrate_guard_test.go`. It is vacuous against the
  embedded set while 001 is the only migration, so the scanner is proved against
  synthetic input: a real drop fails, the same words inside a comment or a
  string pass, the `-- migration-guard: allow <reason>` line passes and covers
  one statement only, with nothing but code allowed between the two.
  The deny list matches bare verbs (`drop`, `rename`, `truncate`,
  `delete from`) rather than the keyword pairs the plan listed: SQLite makes
  `COLUMN` optional, so `ALTER TABLE t DROP b` and `ALTER TABLE t RENAME a TO c`
  destroy for real while `drop column` and `rename to` match neither of them.
  A second test pins the sha256 of both baseline files, since golang-migrate
  records a version integer with no checksum and nothing else would notice an
  edit to a file every existing database was already created from.

`make test`, `make lint`, `make templint` green. Not committed, not staged.
This file is untracked and belongs to another session, which may want to
reconcile its header ("Nothing agreed, nothing built") and the last bullet of
"Explicitly not in this plan" (the onboarding chain has run green, and
`docs/qa/rounds` exists).
