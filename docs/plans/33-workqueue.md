# Workqueue: one durable job runner

Status: agreed 2026-09-12. **Steps 1, 2 and 3 built and verified the same day**
(the table, the package, boot recovery, `config.apply`, and `tile.deploy`).
Steps 4 and 5, backups, volume moves and cron, agreed in detail 2026-09-17
(see "Steps 4 and 5, agreed" below) and **built the same day**. Tests, lint
and templint pass; the panel backup, a tile's Run now and a cron run were driven
locally, and moves, backups and restores on the two-node rig (see "Rig
results").

Step 3 was brought forward because redeploying the panel stranded whatever was
building and repeatedly disrupted verification.

Long-running work is started from an HTTP handler five different ways today,
and each one invented its own machinery. One of them, the config apply, has no
queue at all and runs on the request context, so it dies when the browser stops
waiting. That previously produced half-applied environments when clients
disconnected.

This replaces all five with one table and one runner.

## What exists today

| feature | how it queues | where state lives | survives a restart |
|---|---|---|---|
| deploy | ~~`chan string`~~ `work_items` | `deployments` | **yes**, requeued on boot |
| backup | bare goroutine | `backup_runs` | no |
| cron / functions | the scheduler | `cron_runs` | schedules yes, an in-flight run no |
| volume move | bare goroutine | in-memory map only | no |
| config apply | ~~nothing~~ `work_items` | the plan row, written with a live context | **yes**, requeued on boot |

Only the deploy engine does the job properly: `run()` takes
`context.Background()` with a 30 minute timeout and keeps a cancel handle so
Stop works. Everything here generalises that.

`infra/netcrypt` was a sixth, with its step kept in a settings row so the job
could resume after it restarted the panel. It was deleted on 2026-09-11, but it
is the reason `step` is a column below: it was the only one that survived a
restart, and it had to be, because it restarted the process on purpose.

## The table

Migration `012_workqueue`. New package `infra/workqueue`; `infra/jobs` is
already the cron scheduler and keeps that name.

```sql
CREATE TABLE work_items (
    id           TEXT      PRIMARY KEY,
    kind         TEXT      NOT NULL,           -- config.apply, tile.deploy, ...
    dedupe_key   TEXT      NOT NULL DEFAULT '',-- supersede scope, see below
    payload      TEXT      NOT NULL DEFAULT '',-- JSON, the handler's own shape
    status       TEXT      NOT NULL DEFAULT 'queued',
                                               -- queued | running | done | error | cancelled | superseded
    step         TEXT      NOT NULL DEFAULT '',-- coarse progress, resume point
    progress     TEXT      NOT NULL DEFAULT '',-- JSON, bytes/total and friends
    error        TEXT      NOT NULL DEFAULT '',
    attempts     INTEGER   NOT NULL DEFAULT 0,
    created_at   TIMESTAMP NOT NULL,
    started_at   TIMESTAMP,
    finished_at  TIMESTAMP
);
CREATE INDEX idx_work_items_claim ON work_items (status, created_at);
CREATE INDEX idx_work_items_kind  ON work_items (kind, dedupe_key, status);
```

`dedupe_key` is what `SupersedeWaiting` hand-rolls in the deploy engine today:
enqueueing marks any older `queued` row with the same kind and key
`superseded`. For a config apply the key is the stack id, so two rapid pushes
do not both apply. For a deploy it is the tile id, which is exactly today's
behaviour.

`progress` exists because `volmove` already needs it. Its byte counters live in
a map that a restart loses, and the modal then reads zero.

## The runner

```go
type Handler func(ctx context.Context, j *Job) error

func (q *Queue) Register(kind string, h Handler, opts KindOpts)
func (q *Queue) Enqueue(ctx context.Context, kind, dedupeKey string, payload any) (string, error)
func (q *Queue) Cancel(id string) error
```

- `KindOpts` carries the per-kind timeout (deploy 30 min, apply 15, backup as
  long as it takes) and how many of that kind may run at once.
- Each job runs on `context.Background()` with that timeout, never on a
  request context. A client going away must never stop work that is already
  changing containers and databases.
- A cancel handle per running job, so Stop works the way deploy's does now.
- `j.SetStep` / `j.SetProgress` write through to the row, so the UI polls one
  place for every kind of work.

**Claiming.** One panel process, so claiming is `UPDATE ... SET status =
'running' WHERE id = ? AND status = 'queued'` and checking rows-affected. That
is enough today and stays correct if a second process ever appears, which
matters because panel failover is already on the roadmap (`docs/notes.md`,
`# misc`). No advisory locks, no leases, until there is a second writer.

**Boot recovery.** This is the point of the table. On start, every row left
`running` is either requeued or failed with "the panel restarted", per kind:

- `config.apply` requeues. It is convergent by design, the comment at
  `config/stackconf/apply.go:770` says so.
- `tile.deploy` requeues, same reason.
- `volume.move` fails. It is a two-pass rsync with a service scaled to zero;
  resuming one blind is how data goes missing.
- `backup.run` fails, and the partial artifact is deleted.

A `queued` row just runs.

## Order to build it

1. Table, package, runner, boot recovery. Nothing uses it.
2. Move `config.apply` onto it. This is the broken one: the handler enqueues
   and redirects, the plan page polls the work item, and `SetConfigPlanError`
   finally writes with a live context. Fixes finding 9.
3. Move `tile.deploy`. **Done.** `Engine.WithWork` registers the kind and
   `Engine.dispatch` replaces the channel push; the dedupe key is the tile id,
   which is what `SupersedeWaiting` did by hand. The one wrinkle: the boot
   sweep still errors interrupted rows, because without a queue nothing would
   retry them, so the handler calls `Engine.reopen` to put the row back to
   queued first, and only for a row still `running` or carrying the sweep's own
   message. A cancel, a supersede or a real build failure is left alone.
4. Move `backup.run` and `volume.move`.
5. Cron stays a scheduler; its ticks *enqueue* a `cron.run` item rather than
   running inline.

Steps 2 onward are independent. Stopping after 2 still fixes the bug that
started this.

## Steps 4 and 5, agreed 2026-09-17

Working rules: discuss first, one point at a time, no code without a go, no
git writes, no edits to `*_templ.go` or `output.css`, terse UI copy.

### Where things stand before the change

- `infra/backup/backup.go`: `Service.Run` and `Service.Restore` run inline on
  a `context.WithoutCancel` request context. The web handler
  (`handler/backups/handler.go` Run and Restore, `handler/settings/handler.go`
  RunPanelBackup) and the API (`handlers/api/v1/backups.go` runBackup,
  restoreBackup) block until done. Schedules call `Run` from a robfig cron
  entry. The per-backup claim is the in-memory `running` map
  (`begin`/`end`), shared by run and restore.
- `infra/volmove/volmove.go`: `Start` launches `go s.run`. State is the
  in-memory `moves` map, read by `ForTile`/`All`. Callers:
  `handler/server/nodes.go` (MoveForm, Move, moveError),
  `handler/project/placement.go`.
- `infra/jobs/jobs.go`: cron entries call `RunApp` inline; manual runs call
  `StartApp` (`go s.runApp`). Callers: `handler/app/handler.go`,
  `handlers/api/v1/apps.go`, `cmd/stackrd/main.go` (on-deploy function
  trigger). `Stop` uses the in-memory `cancels` map.
  `CloseOrphanCronRuns` closes rows on boot but the swarm job service keeps
  running.
- Found while reading: a restart mid backup leaves pause-mode containers
  paused, or stop-mode services scaled to zero. Nothing puts them back.

### Decisions

1. **Restore goes on the queue too.** New kind `backup.restore` beside
   `backup.run`. On restart it fails and leaves the service stopped, with an
   error saying the volume may be half-restored. Starting an app on half its
   data is worse than leaving it down.
2. **Run now and Restore return at once.** They enqueue and return. The runs
   list in `handler/backups/panel.templ` shows the running row and polls
   until done. Restore has no row of its own, so its status is read from the
   work item. API `backup run` returns the queued run (CLI already prints
   "Started run X"). CLI `backup restore` keeps its wait by polling the work
   item, so "Restored." still means restored.
3. **Volume move restart.** Fails. `step` records the phase (copy, stop,
   delta, start). Cleanup per step:
   - copy: app still running, close the receivers on the target.
   - stop / delta: close the receivers, scale the service back up on the
     source.
   - start: home node may point at the target. Put it back to the source and
     scale up, the same path `do()` already takes on a failed start.
   - Half-copied target volumes stay, as on any failed move today, and the
     error says so.
   - Progress (bytes, total, phase) moves from the map to the work item's
     `progress`/`step`, so the modal survives a restart. One move per tile
     stays enforced.
4. **Cron restart.** Fails, and cleanup removes the orphaned swarm job
   service (same effect as Stop). No requeue: re-running a half-done job
   blind is not safe. Ticks, manual runs and on-deploy runs all enqueue
   `cron.run`; the `cron_runs` row stays the detail row. Stop becomes
   `Queue.Cancel`. Skip rules (overlap, held stack, depends_on) stay in the
   handler.
5. **Concurrency limits are admin settings, not constants.** Four
   server-only fields on `settings.Settings` (`config/settings/settings.go`),
   in the same JSON blob as `BuildNode` and ignored at lower levels:

   | kind | default |
   |---|---|
   | `cron.run` | 8 |
   | `backup.run` | 2 |
   | `backup.restore` | 1 |
   | `volume.move` | 2 |

   On the server settings form (`serverSettingsForm` in
   `handler/server/server.templ`) next to Build node, and in the API
   catalogue (`settingKeys` in `handlers/api/v1/settings.go`) so the CLI can
   set them. `workqueue.go` stops fixing the semaphore at `Register` and reads
   the limit each drain, so a change applies within seconds. Lowering a limit
   lets running jobs finish; only new ones wait. Kinds without a setting
   (`config.apply`, `tile.deploy`) keep `KindOpts.Concurrency`.
6. **No global work admin page in this pass.** Its own small plan later.

### Implementation choices, 2026-09-17

- The in-memory per-backup and per-cron claims stay inside the handlers. The
  queue's dedupe only touches waiting items, so it cannot stop two running.
- `backup.run` and `cron.run` are keyed by their run row id. The row is
  written at enqueue, so the API returns it and Stop finds the item from it.
  `backup.restore` and `volume.move` are keyed by backup id and tile id.
- New API route `GET /api/v1/backups/:id/restore`, the newest restore. The
  CLI polls it.
- The backups tab polls only the history list, so a schedule being edited
  is not wiped.
- `CloseOrphanCronRuns` is gone: a waiting run survives a restart, and an
  interrupted one is closed by cleanup.
- The Move modal says "Waiting to start" while the move limit is full.
- Volume moves keep an in-memory per-tile claim in the handler as well as
  Start's table check, which a double click can race.
- Restart cleanup is capped at 30 seconds per item, because recovery runs
  before the panel serves and a cleanup may dial a dead node's agent.
- A failed restore shows on the backups tab for 24 hours.
- `KindOpts.Cleanup` returns a message that replaces `RestartFail`, because
  a restore's message depends on whether it had started writing.

### Rig results, 2026-09-17

Driven on the two-node rig with the managed postgres `orgpg`:

- Move both ways: 14 seconds each, progress and phases in the modal.
- Move with a slow start (image not yet on the target): finishes as Moved.
- Move whose start cannot succeed (target paused mid move): rolls back,
  home node, service constraint and status all back on the source.
- Panel restart during the start phase: cleanup sees it running on the
  target and keeps it there, receivers closed.
- Dump and stop-mode volume backups, CLI `backup run` and `backup restore`
  (the CLI waits and prints Restored.), web restore with the tab polling.
- Panel restart mid volume restore: service left at zero, error says so,
  CLI reports the failure.
- Panel restart mid stop-mode backup: service scaled back up, run closed
  with cleanup's reason.

Found and fixed while doing it:

- `runtime.WaitRolled` never settled a service started from zero replicas,
  because swarm records no update then. Every managed-database move waited
  out 90 seconds and failed. `ServiceState.RolledSince` also accepts every
  running task being newer than the change.
- A failed start put `home_node` back but never redeployed, so the service
  stayed pinned to the target, running on the copy, while the panel said the
  source. The next deploy would have gone back to the stale copy and lost the
  writes. The move now waits `startGrace` for a late start, and otherwise
  stops, puts the home node back, and redeploys on the source. Restart
  cleanup does the same.
- `SweepStaleRuns` closed interrupted backup runs before the queue's cleanup
  ran, hiding its reason. Its backup_runs statement is gone.

Not driven: a restart mid copy (46 MB copies in under a second), and backup
cleanup in pause mode.

### Build order

1. Queue reads limits from settings; settings fields, form, API catalogue.
2. `backup.run` and `backup.restore`, with restart cleanup; web page polls;
   API and CLI.
3. `volume.move` on the work item, with per-step restart cleanup; modal reads
   the work item.
4. `cron.run`: ticks, manual and on-deploy enqueue; Stop cancels; restart
   removes the job service.
5. `make test`, `make lint`, `make templint`, then drive the backups page and
   the Move modal in the browser.

## Not doing

- Retries with backoff. `attempts` is in the table so it can be added, but
  nothing here should silently retry: a failed apply or deploy is something a
  person looks at.
- Cross-process locking. One panel today, see Claiming.
- A general scheduler. `infra/jobs` already owns cron and stays.
- Priorities. Kind-level concurrency covers what we need.

## Open

- Does the Runs tab become a global "work" view, or does each kind keep
  rendering its own rows off `work_items`? Per-kind for steps 4 and 5
  (decided 2026-09-17). The admin page listing everything is deferred.
- Deploy logs stream through `stream.Hub` keyed by deployment id. If deploys
  move onto work items, the topic key changes and `logs.js` follows.
- Whether `cron_runs`, `backup_runs` and `deployments` collapse into
  `work_items` or stay as the per-kind detail rows they are. Leaning stay: they
  carry columns a generic table should not.
