# Workqueue: one durable job runner

Status: agreed 2026-09-12. **Steps 1, 2 and 3 built and verified the same day**
(the table, the package, boot recovery, `config.apply`, and `tile.deploy`).
Steps 4 and 5, backups, volume moves and cron, are not started.

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

## Not doing

- Retries with backoff. `attempts` is in the table so it can be added, but
  nothing here should silently retry: a failed apply or deploy is something a
  person looks at.
- Cross-process locking. One panel today, see Claiming.
- A general scheduler. `infra/jobs` already owns cron and stays.
- Priorities. Kind-level concurrency covers what we need.

## Open

- Does the Runs tab become a global "work" view, or does each kind keep
  rendering its own rows off `work_items`? Leaning per-kind, with one admin
  page listing everything.
- Deploy logs stream through `stream.Hub` keyed by deployment id. If deploys
  move onto work items, the topic key changes and `logs.js` follows.
- Whether `cron_runs`, `backup_runs` and `deployments` collapse into
  `work_items` or stay as the per-kind detail rows they are. Leaning stay: they
  carry columns a generic table should not.
