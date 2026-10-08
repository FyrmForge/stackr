# Ready blitz: what stackr needs before the serverconfig move and a prod release

Status: go 2026-10-07 (darthvader). DR itself is parked in `recovery.md`. The serverconfig move itself happens
elsewhere, by hand; nothing here writes into `~/Projects/serverconfig`.

Rules for every worker: no git write ops (no stash either). Never hand-edit
generated files (`*_templ.go`, `ui/static/js/elements/*`, `output.css`,
`docs/openapi.json`, `staticmanifest.go`); regenerate them. Touch only the
files you own; a need elsewhere goes into `ready-seams.md`. No em dashes.
Match neighbouring style; `ponytail:` for ceilings. **Test first**: write the
failing test from the scenario, see it fail, then fix. Finish with
`go build ./... && go vet ./...`, the tests of every package you touched,
and `golangci-lint run` on them.

Every schedule is an admin setting holding a five-field cron expression
(same syntax as volume backup schedules, `CRON_TZ=` allowed); a change
reloads the scheduler at once, no restart.

## Wave 0: contract (one worker)
1. `leaf/settings/catalogue.go`, server scope (`Flat`), validated as cron
   (reuse the volume schedule's parser):
   - `panel_backup_schedule`, default `0 3 * * *`; empty = off.
   - `panel_backup_keep`, int, default `14`.
   - `panel_backup_dest`, a global backup destination id; empty = local.
   - `cleanup_schedule`, default `30 4 * * *` (the existing
     `cleanup_enabled` stays the on/off switch).
   - `orphans_schedule`, default `@daily` (today hard-coded in
     `flow/schedule/schedule.go` `Entries`).
2. `flow/schedule/schedule.go`: `Drivers` gains `PanelBackup` and `Cleanup`
   funcs and a `Settings func(ctx) (map[string]string, error)`; `Entries`
   takes the specs from settings instead of literals (orphans, panel backup,
   cleanup only when `cleanup_enabled`); an invalid stored spec logs and
   skips that entry, never blocks the rest.
3. `internal/service/admin.go` `SetSetting`: a change to any of these keys
   calls `o.sched.Reload(ctx)`.
4. `internal/service/orchestrator.go`: wire `PanelBackup` to enqueue the
   existing `kindPanelBackup` job (payload marks it scheduled) and `Cleanup`
   to a new `kindCleanup` job whose handler is a stub returning nil, in a
   new file `internal/service/cleanup.go`.
5. VIP boot hook: new file `internal/service/vipboot.go` with
   `func (o *Orchestrator) rebuildVIPs(ctx context.Context) error` returning
   nil (R7 fills it); `service.New` calls it at boot after `jobs.Start` and
   before the first deploy can run, logging (not failing) an error.
6. Tests: `Entries` with defaults, with an override, with cleanup off, with
   a bad spec; SetSetting reloads.
7. `make test && make lint` green; hand-off section in `ready-seams.md`
   listing the knob names, the Drivers fields, the job kinds and payloads.

## Wave 1: six workers in parallel

### R1 Panel backups on a schedule, off the box
Owns: `flow/backup/backup.go` (`PanelBackup`), `cmd/stackr-install/restore.go`, `internal/service/backup.go`
(panel parts), `internal/service/jobs.go` (panel backup handler only), the
admin drawer Backups tab (`ui/drawer/admin/admin.templ` backups section,
`web/handler/admin/handler.go` backups section), tests.
- The scheduled run reads `panel_backup_dest` (a global destination; local
  when empty or the dest is gone, with a warning in the job log) and
  `panel_backup_keep`, and prunes to keep at that destination.
- A scheduled run's archive is marked `schedule` in the runs list.
- The archive also carries the proxy's data volume (`stackr-caddy`:
  certificates, their keys, ACME accounts) via the existing volume tar
  tool, so a restore on a new box does not re-issue every certificate
  (Let's Encrypt limits). `stackr-install restore` puts it back before the
  proxy starts. Test: archive lists the caddy tree; restore writes it.
- Admin drawer Backups tab shows the schedule, keep and destination (a
  select of global dests) as editable settings, plus "last run" and its
  status.
- Restore stays `stackr-install restore <archive>`; for an S3 dest, document
  the fetch step in `docs/` (where install docs live; grep).
- Tests: scheduled run goes to the S3 fake dest and prunes to keep; a
  deleted dest falls back to local with the warning.

### R2 Disk cleanup
Owns: `internal/service/cleanup.go`, `docker/images.go` (prune helpers),
`leaf/image/*` (keep set), `docker/docker.go` interface lines for new calls,
`dockerfake/fake.go`, tests.
- Handler: when `cleanup_enabled`, remove (a) dangling images, (b)
  stackr-built images not referenced by any running container nor by the
  last N releases of any env (N = the rollback depth the release list shows;
  find it, else 5, `ponytail:`), (c) Docker build cache older than 7 days
  (`BuildCachePrune` with an `until` filter). Never touch images of
  containers stackr does not manage, never touch volumes.
- Job log: one line per category with counts and bytes reclaimed.
- Tests: keep set keeps running and rollback-reachable images, removes the
  rest; cleanup off does nothing; foreign images untouched.

### R3 Job state bugs
Owns: `flow/promote/promote.go` (Apply and resume path), `flow/jobs/jobs.go`,
`leaf/job/*`, the `ParamSet` hook in `internal/service/orchestrator.go`
(only that block; W0 owns the schedule wiring lines), tests.
1. A promote or rollback that parks at deploy time after `SetRelease` moved
   the env pointer must, on resume, deploy every tile whose running image
   differs from the env's release, not finish Done on an empty diff. Same
   for an `errs.Unset` park.
2. `Requeue` only changes a row still waiting (conditional update); a
   superseded or cancelled job is never brought back.
3. `ParamSet` returns true for a host-access park whose ask the stack's
   current grant now covers, so it resumes without a second approve.
- Tests: each scenario failing first (promote parks on host access after
  SetRelease, approve, resume deploys the new image; requeue of a cancelled
  job is a no-op; covered park resumes).

### R4 Health ordering in depends_on
Owns: `flow/deploy/deploy.go` (wait logic and single-tile ordering),
`flow/promote/plan.go` and `promote.go` only where rollout order consumes
the condition, `leaf/tile/check.go` `ParseDep` if its return needs a type,
tests.
- `db:healthy` waits until the dependency's containers report healthy
  (Docker health), `init:completed` until the one-shot or function tile's
  last run exited 0, plain `db` keeps today's started semantics.
- Timeout: the dependency's healthcheck start period plus retries times
  interval, capped at 10 minutes; on timeout the dependent fails with
  "waited for db to be healthy; it is not" and the release is not marked
  deployed.
- A single-tile deploy whose dependency is not running/healthy waits the
  same way (today it does not order at all).
- `:completed` on a function tile with trigger `on_deploy` runs it first.
- Tests: healthy wait passes, times out, completed wait, cycle still refused.

### R5 Secrets import
Owns: `cmd/stackr/params.go` (or the params command file; grep), its test,
`docs/` CLI reference if one exists.
- `stackr params import <file> --scope org|stack|env [--stack S --env E]
  --collection C [--secret] [--dry-run]`: reads a `.env` file (KEY=VALUE,
  `#` comments, quoted values, `export` prefix, no interpolation), names
  lowercased to the param name rules (refuse what cannot map, list them),
  one `SetParams` call (it already takes many entries), prints the redeploy
  lines like `params set`. `--dry-run` prints names only, never values.
  Values never echo; refuse an unreadable or world-readable file with a
  warning, not an error.
- Tests: parsing edge cases, dry run prints no values, the API call body.

### R6 Security sweep (report only)
Owns: `docs/security/sweep-2026-10-07.md` (new). No code changes.
- Run `gitleaks detect` (git history and tree), `govulncheck ./...`,
  semgrep via `docker run --rm -v "$PWD":/src semgrep/semgrep semgrep scan
  --config p/golang --config p/secrets /src`, trivy via `docker run --rm -v
  "$PWD":/src aquasec/trivy fs /src` and against the built image
  (`ghcr.io/fyrmforge/stackr:0.6.0-dev.15` if present locally, else skip).
  Remove the two scanner images afterwards.
- Triage every finding: real, false positive (why), or accepted; for real
  ones the file, the risk in one line, and a fix sketch. Severity order.
- No fixes in this wave; darthvader picks.

### R7 VIP rules rebuilt at boot (live bug)
Owns: `internal/service/vipboot.go`, `internal/service/internal/vip/*`,
`leaf/tile/world.go` (reads only), tests.
- Today `vip.Rebuild` has no caller (`leaf/tile/world.go:224` claims it
  runs at boot). After a panel restart or upgrade the first deploy
  redeclares `STACKR-VIP` and wipes every other tile's jump; after a host
  reboot all rules are gone. Rebuild the table at boot from Docker: every
  tile's pause container address and its running replicas' addresses on the
  env network, the same reads `Route` uses, then one `Rebuild`.
- Tests (fake Docker): two tiles routed, simulated restart (fresh Table),
  boot rebuild, then deploying tile A keeps tile B's rule.

### R8 Managed Postgres dump covers the slices (data loss)
Owns: `flow/managed/postgres.go`, `flow/managed/flow.go` (dump/restore
paths), `internal/service/backup.go` (method default only), tests.
- Today `dump` runs `pg_dump` on the admin DB only; slices are separate
  databases, so app data is missing from every dump. Make `dump` cover every
  database of the instance (`pg_dumpall` with roles, or one `pg_dump` per
  slice database in one archive; pick the one whose restore is a clean
  wipe-and-load and say why), and make restore load it back with the slice
  roles and grants intact.
- Note in the archive or run row which format it is, so an old admin-only
  dump still restores (as before) and is labelled as admin-only in the UI.
- Tests: argv for backup and restore; a service test that a dump of an
  instance with two slices names both. Rig test in wave 3 against a real
  Postgres: write rows in a slice, dump, wipe, restore, rows back.

## Wave 2: seams and review
One worker: resolve `ready-seams.md`, regenerate templ and openapi, `make
test && make lint && make templint` green, docs (`PROGRESS.md` dated
section, `leftovers.md` strike what landed). Then an Opus review round per
area (R1-R5) like the blitz: real defects only, failure scenario each; a fix
round on what survives verification.

## Wave 3: rig
Ship `v0.6.0-dev.16`; set the panel backup schedule to every 5 minutes with
a global S3 dest if one exists on the rig (else local) and see two archives
and the prune; run cleanup by setting its schedule a minute ahead and read
the job log; a stack with `db:healthy` on a slow-start Postgres; a parked
promote resumed after approve redeploys; `params import --dry-run` on a
sample file; restart the panel container, deploy one tile, and a second
tile's service name still answers; managed Postgres dump and restore with
slice rows intact. Never edit shop, web, infra, byhand. Report in
`docs/qa/ready-2026-10-07.md`.

## Not in this plan
Org roles, alerts and notifications, backups of host-path and share data,
PR env own values, the volume file browser (next), the serverconfig move
itself.
