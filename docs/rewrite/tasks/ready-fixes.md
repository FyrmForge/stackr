# Ready blitz fix round: Opus review findings

Status: go 2026-10-07 (darthvader). Six workers in parallel, then a Fable
verification pass, then rig v0.6.0-dev.16.

Rules: same as `ready-blitz.md` (no git writes, no stash; no hand edits to
generated files; no em dashes; match style; `ponytail:` for ceilings).
**Test first**: each finding gets a test from its failure scenario that FAILS
on the current code (a real assertion failure, not only a compile error;
if the API must change, land the signature first with the old behaviour,
see the test fail, then fix). Touch only your files; shared files below
name the functions each worker may edit; re-read a shared file right
before every edit. Needs elsewhere go to `ready-seams.md` "Fix round needs".
Only G2 runs `templ generate` and regenerates `docs/openapi.json`.

## G1 Disk cleanup
Owns: `internal/service/cleanup.go`, `docker/images.go`, `docker/docker.go`
(interface lines), `internal/service/docker.go` (interface lines),
`leaf/image/*`, `leaf/release/*`, `dockerfake/fake.go`, tests.
1. HIGH. Keep set misses releases of queued, waiting or running jobs (a
   parked promote's release falls out of the top 5, its image is removed,
   the image row delete nulls `release_tiles.image_id`, and the release can
   never deploy: "has no build in release #N"). Keep every `release_id` of
   a job in queued, waiting or running state.
2. HIGH. Rollback depth is per stack, not per env, and deleting the image
   row nulls the pin in every old release for good. Count depth per env
   (the last 5 releases each env was on, from done promote/rollback jobs'
   `release_id` plus the current one). Never delete an images row that a
   release still references: remove the Docker image only, keep the row;
   deploy of a built pin whose image is gone fails with "the image for
   <tile> was cleaned up; rebuild it" (not "build a commit first").
3. MEDIUM. Rows are deleted even when the Docker removal failed or was an
   in-use conflict, and a partial error skips all row work. Delete only rows
   of images actually removed (`PruneImages` returns removed ids).
4. MEDIUM-LOW. A missing `stackr-builder` fails the job every night: treat
   "no builder" as 0 removed.
5. LOW. Log counts images not tags; bytes say "up to" (shared layers);
   `parsePrune` ignores the header and stderr lines.
6. Note only (no fix): pulled image-watch digests are never removed; the
   `stackr.built` label is not scoped to one install. Record in leftovers.

## G2 Panel backups, schedules, library bumps
Owns: `flow/backup/backup.go` ONLY `PanelBackup`, `writePanel`,
`panelCaddy` and helpers they alone use; `internal/service/backup.go` ONLY
`panelBackup`, `PanelBackupNow`, `PanelBackups`; `leaf/backup/*` (Prune,
ObjectKey, PanelPrefix, dest delete check); `flow/schedule/*`;
`leaf/settings/*`; `internal/service/admin.go`; `internal/service/jobs.go`
panel backup handler only; `internal/service/orchestrator.go`; admin
drawer templ and handler (Backups tab); `cmd/stackr-install/*`;
`internal/installspec/*`; `cmd/stackrd/main.go`; `go.mod`, `go.sum`;
`cmd/stackrd/Dockerfile`; `docs/features/panel-backups.md`; tests.
1. HIGH. Every install uses install id "default", so two boxes on one
   bucket prune each other's archives. The installer generates a random
   install id, saves it in the install answers and sets `STACKR_INSTALL_ID`
   on the panel container; an install without one keeps "default" (no
   users: no migration needed beyond that).
2. MEDIUM. Manual and scheduled runs share kind and lock, so one supersedes
   and cancels the other; a cancelled run's row stays `running` forever (the
   finish uses the cancelled ctx). A scheduled run is never cancelled by a
   manual one (separate supersede key, or a manual click while one runs is
   refused "a panel backup is running"); `Backups.Finish` uses a
   non-cancelled ctx with a short timeout. Lock panel backups against the
   upgrade (`panel` lock) so they never overlap.
3. MEDIUM. Prune ignores the trigger: a scheduled run can delete the
   pre-upgrade archive. Archives go under per-trigger prefixes
   (`scheduled`, `manual`, `upgrade`); `panel_backup_keep` applies to
   scheduled only; manual and upgrade keep their own fixed counts (the old
   14). Prune never deletes the key it just wrote.
4. Agreed with darthvader: an on/off switch `panel_backup_enabled` (default
   on) and `orphans_enabled` (default on); an empty schedule box means the
   default time; drop the `off` magic word. Backups tab shows the switch.
5. LOW-MEDIUM. Deleting a backup dest that `panel_backup_dest` names is
   refused like dests a volume schedule uses. The Backups tab "Last run"
   line shows where it went.
6. LOW-MEDIUM. Boot clears `<data>/backups/scratch` (unencrypted db and
   cert tars left by a crash).
7. LOW. Scheduler `load` reads settings inside the mutex; a settings read
   error keeps the previous table instead of defaults.
8. LOW. The Backups form validates all three values before saving any, and
   only reloads/re-syncs once.
9. LOW. Restore keeps a copy of the current `stackr-caddy` tree and puts it
   back if the untar fails; it deletes its staging dir after a successful
   restore (it holds unencrypted cert keys).
10. Library bumps (darthvader yes): `google.golang.org/grpc` to the first
    fixed version, `github.com/prometheus/prometheus` to v0.311.3 (or the
    nearest that builds), `golang.org/x/mod` to v0.40.0, and `apk upgrade
    --no-cache` in the Dockerfile's final stage. Rerun `govulncheck ./...`
    and note what remains in `docs/security/sweep-2026-10-07.md`.

## G3 Managed Postgres restore safety
Owns: `flow/managed/postgres.go`, `flow/managed/flow.go`,
`flow/backup/backup.go` ONLY the volume restore and dump branches
(not the panel functions), `internal/service/backup.go` ONLY `subject`,
`RestoreBackup`, `runRestore` and holder logic, `docker/exec.go`, tests.
1. HIGH. The role filter `grep -Ev "^(CREATE|ALTER) ROLE ..."` runs over the
   whole stream and deletes COPY rows and function body lines (proven: 3
   rows in, 1 out, exit 0). Filter only before the first `\connect` line.
2. HIGH. Apps writing during the load break the restore after the wipe
   (duplicate key, PK never built, later databases never loaded). Stop every
   tile that consumes the instance (slice tiles' consumers and bound tiles)
   before the wipe, restart them after, success or failure; the job log
   names the pre-restore backup run on failure.
3. HIGH. A cluster dump restored onto a different instance drops that
   instance's slices and loads names it does not track. Refuse a
   cluster-dump restore across instances ("restore a cluster dump onto the
   instance it came from").
4. MEDIUM. Slices or bindings created after the backup are dropped or
   broken. Refuse with the list, or require a `--force`-style confirm, when
   the instance has provisions or bindings newer than the run.
5. MEDIUM. stderr is interleaved into the dump archive (`exec.go`
   `MultiWriter(pw, &stderr)`), so a pg_dumpall warning becomes a syntax
   error at restore. Keep stdout and stderr apart for backup streams.
6. MEDIUM. Restore and deploy can overlap (Provision or Bind during the
   load). The restore job also takes the instance tile's lock.
7. Notes only, record in leftovers: cross-database consistency of
   pg_dumpall during a concurrent provision; `\restrict` needing psql
   17.6+ on a moving `postgres:17` tag; the UI label for old admin-only
   dumps.
Prove 1-3 in a real postgres container under the session scratchpad
(`/tmp/claude-1000/-home-darthvader-FyrmForge-stackr/a5e3e10d-036f-4138-a78a-2104ba405d5a/scratchpad/g3/`), removed after.

## G4 Health ordering
Owns: `flow/deploy/deps.go` and tests, `flow/deploy/deploy.go`,
`flow/promote/promote.go`, `internal/service/run.go`,
`internal/service/jobs.go` (deploy, promote, env-sync handlers only),
`flow/run/*`, tests.
1. HIGH. `runFirst` queues a run row then `Do` returns on `errs.Unset`
   before `Begin`, leaving a Queued row that blocks the function for good.
   Never leave a row: resolve the spec before queueing, or close the row on
   any early return. A restart between Queue and Begin: `Interrupted`
   closes Queued rows with no job too.
2. MEDIUM. Waits inside a promote or sync run with `swapping=true`, so they
   cannot be cancelled or superseded. Wait first, swap after (per tile, as
   Redeploy already does).
3. MEDIUM. A dependency failure leaves the env on the new release. Keep
   `SetRelease` where it is (R3's resume relies on it) but make the job fail
   with a message that says the env is partly rolled out and a re-promote
   finishes it; record the design note in leftovers.
4. MEDIUM. `:completed` on a function whose run is queued behind the
   promote's own lock waits 10 minutes: fail at once with "migrate has a run
   queued behind this deploy; let it finish and promote again".
5. MEDIUM. `:healthy` on a dependency with no replica containers (slice,
   cron, function, never deployed) waits the full timeout: fail at once
   with "db has nothing running"; count only running replicas (an exited
   leftover does not fail the check).
6. MEDIUM. Single-tile waits hold a worker while the dependency's own deploy
   is queued behind them: fail at once when a deploy of the dependency is
   queued.
7. LOW. `runFirst` honours the ranFirst marker (two dependents on one
   function run it once; a resume does not rerun it), and waits for an
   active run instead of a Conflict. Health timeout reads the image's own
   HEALTHCHECK when the tile sets none; explicit retries 1 or 2 are kept.

## G5 Job state
Owns: `flow/promote/plan.go`, `flow/deploy/spec.go` (labels only),
`flow/jobs/*`, `leaf/job/*`, `store/jobs.go`, `internal/service/hostgrant.go`,
tests.
1. MEDIUM. `runsOther` misses tiles running by tag (every image tile until
   its second deploy), so a parked promote still ends Done on the old image.
   Label each replica at create with the pinned ref (`stackr.ref`) and
   compare the label; containers without it fall back to today's check.
2. LOW. On resume, on_deploy functions whose pin changed are not in
   `plan.Deployed`, so their run is never queued (a migration skipped).
3. MEDIUM. `ApproveHostGrant`'s `Requeue` runs outside the runner lock and
   can resurrect a superseded job. Add `JobStore.RequeueIfWaiting`
   (`UPDATE ... WHERE id=? AND state='waiting'`) and use it everywhere
   `Requeue` is used.
4. LOW. `run()` parks without the lock, after a cancel the job is still
   parked. Park conditionally too (`WHERE state='running'`), or check the
   cancel cause under `r.mu`.

## G6 VIP boot rebuild
Owns: `internal/service/vipboot.go`, `internal/service/internal/vip/*`,
`internal/service/jobs.go` ONLY `watchTick`, tests.
1. MEDIUM-HIGH. The rebuild shares the 20 s boot ctx; a slow Docker fails
   `jobs.Start` and the panel does not boot. Give it its own ctx
   (`context.WithoutCancel` plus its own timeout); boot never fails on it.
2. MEDIUM. A tile skipped by a transient error drops its still-correct
   kernel rules. If any tile was skipped for an error (not for being gone),
   do not shrink: keep its previous entries or skip the Rebuild and log.
3. MEDIUM. After a host reboot the rebuild can run before containers are up.
   `watchTick` re-routes every minute (same reads); a ponytail note on
   cost for big installs.
4. LOW. One non-IPv4 address empties the whole table: validate per tile in
   the collector.
5. Test gap: a pause container whose tile row is missing.

## Verification (one Fable worker, after all six)
Replay every finding above with its own scratch tests (Postgres findings in
a real container), close seams, `make test && make lint && make templint`,
then rig `v0.6.0-dev.16`: panel backup schedule every 5 min to a global
dest (or local), two archives, prune by trigger, upgrade archive survives;
cleanup on with a parked promote (its image survives) and a per-env rollback
target (survives); a `db:healthy` dependency on a slow Postgres; a parked
promote resumed redeploys a tag-started image tile; restart the panel
container and service names still resolve; managed Postgres dump with two
slices and a COPY row starting `ALTER ROLE`, restore with a consumer
writing, rows intact. Never edit shop, web, infra, byhand. Report in
`docs/qa/ready-2026-10-07.md`.
