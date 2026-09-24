Source: internal/stackrd/infra/workqueue/ (workqueue.go, workqueue_test.go)
Commit: c2423f0
Taken: the durable-job idea only — row shape, state set, how a worker claims, boot recovery, cancel, where a run's output goes
Cut: all of the code — the plan re-does it as flow/jobs with a lock set per job, N workers, the Railway supersede rule and a `waiting` state
Cuts belong to: flow/jobs (runner, supersede, lock set), leaf/job (the row and its states)

## Why it is a table

Long work used to start from an HTTP handler five different ways. The config
apply ran on the *request* context: a browser that stopped waiting killed it
wherever it had got to, and the error it tried to write used that same dead
context, so nothing was recorded. The webhook path was worse — GitHub hangs
up after about ten seconds.

Two rules are the whole idea worth copying:

1. A job runs on `context.Background()` with its own timeout, never on the
   context of the request that asked for it.
2. The queue is a table, so a row left `running` by a restart is dealt with
   on boot instead of vanishing with the process.

## The row

id, kind, dedupe_key, payload (JSON, kind-specific), status, step, progress
(JSON blob), error, attempts, created_at, started_at (nullable),
finished_at (nullable).

`step` is coarse progress *and* the resume point for a kind whose recovery
is "carry on from here" — that is why it is a column, not a log line.
`progress` is the fine blob this kind's UI reads (bytes done and total, and
friends). `error` holds the reason in words: a failure someone has to act on
belongs on the row, not only in a log.

## States

- `queued` — what enqueue writes; the only state a worker may claim.
- `running` — claimed; started_at set, attempts bumped.
- `done`, `error`, `cancelled` — terminal. `error` and `cancelled` are two
  spellings on purpose: an interrupted or failed run is not someone pressing
  Stop, and nobody should have to investigate the second one.
- `superseded` — terminal, set on the older row when a newer job for the
  same key arrives. Never `error`, for the same reason.

## Claiming

Claim is one conditional UPDATE, `queued → running` by id, reporting whether
it changed a row. That is the entire lock: two claimers race on it, exactly
one wins, and that is what stops a job running twice. No leases, no
heartbeats, no worker registry.

## Boot recovery

On start, before the panel serves, list every row still `running` — nobody
can actually be running, the process that owned them is gone — and per kind
either:

- **requeue** it (back to `queued`, started_at cleared) when the work is
  convergent: re-running it against a half-finished state finishes the job;
- **fail** it, with a message that says what to do next rather than "the
  panel restarted". For work that cannot be resumed blind: a two-pass rsync
  with the service scaled to zero, a half-written backup artifact. A
  bounded, best-effort cleanup hook runs first (delete the partial artifact,
  put the service back up) and may replace the message. It must not hold
  boot hostage.

A row whose kind nobody registers any more is failed, not left alone: a
`running` row hides from every listing of what is wrong.

## Cancel

The runner keeps an in-memory `id → context.CancelFunc` for what it is
running. Cancel finds it and calls it; the handler returns, and the
finishing write records `cancelled` rather than `error` because the run
context ended in Canceled, not in a handler failure. No entry in the map
means the job is not running here: write `cancelled` on the row unless it is
already terminal. A job that already finished is not an error — the caller
is racing the work, which is the normal case for a Stop button.

The finishing write always uses a **fresh** background context. The run's
context is dead exactly when there is something worth recording.

A panic in a handler is recovered: the stack to the log, a short `panic: ...`
to the row. One bad handler must not take the panel with it.

## Per-job output

There is no per-job log file in this package. A run's output is the three
columns above plus whatever the handler writes to the process log. If
`flow/jobs` wants a file per job, that is new work, not something to lift.

## Type sketch

```go
// leaf/job owns this row. flow/jobs is the only thing that writes status.
type Job struct {
	ID         string
	Kind       string // deploy, promote, rollback, restart, stop, backup, ...
	Status     string // queued|running|waiting|done|error|cancelled|superseded
	Payload    []byte // kind-specific
	Step       string // coarse progress, and the resume point
	Progress   []byte // fine blob for this kind's UI
	Error      string
	Attempts   int
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// Claim flips queued → running by id and reports whether it won the race.
// That one UPDATE is the only lock on a row.
func Claim(ctx context.Context, id string) (bool, error)
```

## Notes for the builder

- The old dedupe was one `dedupe_key` string per row (the stack for a config
  apply, the tile for a deploy) and the rule was "newest queued row wins,
  older ones go `superseded`". The plan replaces both with the lock set per
  job and the Railway rule, which also cancels a running job still in its
  build phase. Keep the name `superseded` — it is already the
  not-a-failure spelling.
- Concurrency was capped *per kind*, with the limit re-read from settings on
  every drain pass so a change applied within one poll, and running jobs
  left to finish when it was lowered. The plan has one N-workers setting
  instead; the part worth keeping is re-reading it each pass, not at boot.
- The loop polled every 5s and also woke on enqueue, so a row written by
  anything that did not go through this process (boot recovery, a second
  writer) is still picked up. The plan's `waiting` state needs that poll
  regardless.
- A freed worker slot poked the loop, so the next job starts then and not at
  the next tick.
- The recovery choice was per kind, fixed at registration. Same in the plan:
  deploy is convergent (requeue); a volume copy or a backup is not (fail
  with a message that names what to do).

Size: source 712 lines, extract 136 lines
