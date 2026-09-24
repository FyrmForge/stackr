# Extract: the cron runner behind `service/scheduler`

Source: `infra/jobs/jobs.go`; the schedule-loading half of `infra/backup/backup.go` (`Service` cron fields, `NewService`, `LoadSchedules`); `store/repo/models.go` `Backup.Schedule()`
Commit: c2423f0
Taken: the cron library and where an expression is parsed, the entry registry and how a reload rebuilds it, the running-schedule guard, the missed-tick policy as the repo actually states it, timezone composition.
Cut: cron *tiles* whole (v1 has no cron tile kind) — the run rows, the one-shot container, `Hold`/`Release`, `depends_on` skipping, the work-queue registration; every store call; every Swarm call.
Cuts belong to: `later` (the cron tile kind), `flow/jobs` (the durable queue and its restart policy), `leaf/backup` + `leaf/volume` (the schedule rows), dropped outright (Swarm).

---

## There is no tick loop to port

Both schedulers are `github.com/robfig/cron/v3`. A `*cron.Cron` is created and
started in the constructor, and every schedule is an `AddFunc` entry whose id
is kept in a map so a reload can `Remove` it. The parsing, the timer and the
timezone handling are all the library's; the repo's own code is the registry
around it. `svcerr-scheduler.md` asked for a tick loop — this is the answer:
don't write one.

```go
// Both services hold the same three fields and nothing else scheduler-shaped.
type Service struct {
	mu      sync.Mutex
	cron    *cron.Cron
	entries map[string]cron.EntryID // schedule id -> entry
}

// The cron is started at construction, before any schedule is registered.
// AddFunc on a running cron is safe and takes effect on the next tick, which
// is what lets a reload happen while the process serves traffic.
func New() *Service {
	s := &Service{cron: cron.New(), entries: map[string]cron.EntryID{}}
	s.cron.Start()
	return s
}
```

## Parsing, and the only two questions asked of an expression

Both live in `infra/jobs`; `infra/backup` has neither and relies on the same
`ValidateCron` at save time.

```go
// ValidateCron rejects bad expressions at save time.
func ValidateCron(expr string) error {
	_, err := cron.ParseStandard(expr)
	return err
}

// NextRun returns the next fire time for a cron expression, zero if invalid.
// Zero, not an error: every caller of this is a UI that draws "next run" and
// has nothing to say about a malformed expression a save already refused.
func NextRun(expr string) time.Time {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}
	}
	return sched.Next(time.Now())
}
```

`cron.ParseStandard` is the 5-field parser plus `@`-descriptors, and it is what
understands the `CRON_TZ=` prefix below. The same expression string is handed to
`AddFunc`, so what validates and what fires are parsed by the same grammar.

## Reload is a full rebuild, never a diff

Both `LoadSchedules` bodies are the same four moves, under the mutex: remove
every registered entry, reset the map, walk the schedules, re-add the enabled
ones. An expression that fails to parse is skipped silently — the save-time
`ValidateCron` is the guard, and a row that got in some other way must not take
the whole reload down with it.

```go
// Reload (re)registers every enabled schedule. Called at boot and after any
// write that changed a schedule.
//
// Rebuild, not diff: the old entry ids are the only handle on what is
// registered, and an edited expression has to lose its old entry anyway. A map
// of a few dozen entries costs nothing to rebuild and a diff is one more thing
// that can drift from the rows.
func (s *Service) Reload(scheds []Schedule) error {
	// extract: schedule list now passed in (was s.store.ListBackups /
	// s.store.ListTiles)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.entries {
		s.cron.Remove(id)
	}
	s.entries = map[string]cron.EntryID{}
	for _, sc := range scheds {
		if !sc.Enabled || sc.Cron == "" {
			continue
		}
		sc := sc
		id, err := s.cron.AddFunc(sc.Expr(), func() {
			if err := s.fire(context.Background(), sc.ID); err != nil {
				slog.Error("scheduled run not queued", "schedule", sc.ID, "error", err)
			}
		})
		if err != nil {
			continue // invalid expression; ValidateCron guards new saves
		}
		s.entries[sc.ID] = id
	}
	return nil
}
```

Two things the closure does *not* do, in both originals: it does not run the
work, and it does not block. It enqueues (`backup.Start`, `jobs.StartApp`) and
returns, so a schedule that fires while the previous run is still going never
holds the cron's goroutine. A tick that cannot even be enqueued is logged and
dropped; there is no retry.

```go
// extract: dropped the enqueue bodies (backup.Start / jobs.StartApp: open a run
// row, then work.Enqueue), belongs in flow/jobs — the closure calls one
// function and logs what it returns, which is the whole contract here.
```

## Not double-starting a running schedule

Two independent mechanisms, and the rewrite needs both — they answer different
questions.

**In the runner**, a mutex-guarded set of refs mid-run. The check is inside the
run, not at enqueue time, because a queued item can wait arbitrarily long behind
the concurrency limit:

```go
// begin marks a ref as running. Returns false when it already is and
// overlapping is not allowed, the caller should skip this tick.
func (s *Service) begin(ref string, allowOverlap bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[ref] && !allowOverlap {
		return false
	}
	s.running[ref] = true
	return true
}

func (s *Service) end(ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, ref)
}
```

A refused tick is not an error: it closes its run as `"skipped"` with
`"previous run still in progress"`, so the history says what happened instead of
going quiet. `allowOverlap` is a per-schedule opt-out.

**In the queue**, the run id is the dedupe key, and a settings-driven `Limit`
caps how many run at once. `infra/backup` holds its own per-volume claim on top
of both (`backup-infra.md`, "Per-volume claim"), because a run and a restore
racing on one volume is not an overlap the runner's ref set can see.

## Timezone

One column, composed into the expression; never parsed back out.

```go
// Schedule is the cron expression as the scheduler sees it: robfig/cron reads
// the timezone off a CRON_TZ= prefix, so the zone is composed in, not parsed.
func (b *Backup) Schedule() string {
	if b.Timezone == "" {
		return b.Cron
	}
	return "CRON_TZ=" + b.Timezone + " " + b.Cron
}
```

No zone means the cron's own location, which `cron.New()` sets to
`time.Local` — the panel host's zone, not UTC.

## Rules as today (spec)

- **Validate at save, never at fire.** `ValidateCron` on the way in; a reload
  skips what will not parse rather than failing.
- **The cron is started once and lives for the process.** Reloads add and
  remove entries on a running cron; nothing stops and restarts it.
- **A tick enqueues, it never runs the work.** The cron goroutine must not be
  the thing a 24-hour volume archive is holding.
- **A missed tick is missed.** Nothing in either file catches a tick up: no
  last-fired column is read at load, no backfill after a reload, no retry on a
  failed enqueue. A restart mid-run *fails* that run rather than requeueing it
  (`OnRestart: workqueue.Fail`, recorded as "stackr restarted while this run was
  in progress") — the stated reason is that re-running a half-done job blind is
  not safe. The same reasoning covers the gap: a schedule that should have fired
  while the panel was down fires next at its next real tick.
- **A reload is idempotent and takes the mutex.** Two concurrent reloads cannot
  leave a doubled entry.
- **Overlap is a per-schedule property**, decided inside the run against a live
  set, not by the queue.

## Notes for the builder

- **Target:** a scheduler under `service/internal/flow/` (step-3 task 23),
  driving backup schedules, the orphan-retention daily job and image watch.
  `svcerr-scheduler.md` already owns the *reload error policy* (post-write
  reload logs and swallows because the row is committed; boot reload returns
  because nothing is serving yet) — this row owns the mechanism it calls. Don't
  write that prose twice.
- **Keep `robfig/cron/v3`.** It is already in `go.mod`, it parses what the saved
  expressions are written in, and it is where `CRON_TZ=` support comes from.
  Replacing it with a hand-rolled ticker buys nothing and loses the parser.
- **DECIDE — how a non-cron driver expresses its schedule.** Only backups carry
  a cron expression today. Orphan retention is "a daily job" in REWRITE.md and
  image watch has its own interval; neither exists as a cron row in the old
  code. Either they get an `@daily` / `@every 1h` descriptor entry through the
  same registry (the library accepts both), or the scheduler grows a second
  kind of entry. Nothing here decides it, and nothing here should be read as
  precedent.
- **DECIDE — the timezone asymmetry.** A backup has a `timezone` column and the
  prefix is composed for it; a cron tile has no such column and the user types
  `CRON_TZ=…` into the expression field by hand (the old form label literally
  says so). One shape should win in the rewrite. Composing from a column is the
  one that survives a timezone rename and can be rendered in a picker.
- **Verify the missed-tick half against the library.** The repo facts above are
  solid. What the library does on a clock jump, a suspend/resume or an NTP step
  is `robfig/cron`'s own behaviour and was not read for this row.
- **`Hold`/`Release` is worth remembering, not porting.** The old runner parks a
  stack's schedule ticks for the length of a config apply, because a cron firing
  mid-apply runs against tiles that do not exist yet (manual runs are not held —
  the person asked, and `Release` undoes exactly one `Hold`). None of v1's three
  drivers has a config-apply race. If a cron tile kind ever lands, this is the
  first thing it needs back.
- **`depends_on` skipping goes with the cron tile** (`skipReason`/`depBlocked`:
  `started`/`healthy` want the dependency running, `completed` wants its last
  run ok, a missing dependency is the deploy's problem). Same reason: no v1
  driver has dependencies.
- **Everything else in `infra/jobs` is a cron tile, a store call or Swarm** —
  `runApp`, `jobSpec`, `pullAuth`, `runNames`, `envLines`, `appImage`,
  `RunService`, `startRun`/`finishRun`, `StartApp`, `WithWork`, `Stop`, the
  triggers, the output tail, the `Runs`/`Deploys` interfaces. Marked `later` as
  a set, not line by line.
- **Say "tile".** The old code's `app`/`StartApp`/`"app:"` ref prefix is the
  same noun. If the new runner keeps a ref set, the key is opaque — no kind
  prefix baked into it.

Size: source 667 lines, extract 243 lines
