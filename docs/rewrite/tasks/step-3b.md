# Step 3b: cron and function tiles

Read first: `docs/rewrite/PROGRESS.md`, REWRITE.md "Rules from the first
commit", `AGENTS.md` "Layering rules", `docs/rewrite/verbs.md`. Extracts:
`tilelifecycle.md` (refusal vocabulary, Stop/Restart/ToggleCron/RunNow
rules), `runpolicy.md` (per-kind whitelist), `cron-runner.md` (robfig
cron, validate at save, a tick enqueues), `tile-crud.md` (`CronReload`
effect), `001_initial.md` (`tiles.cron`, `run_on_deploy`, `last_run_at`,
`cron_runs`). Code: `internal/service/internal/leaf/tile`, `flow/deploy`,
`flow/schedule`, `flow/jobs`, `internal/api`, `internal/cli`.

Why this step exists: v1 fixed tile kinds in step 1 as service and
managed. Cron and function were v0 kinds and were wrongly parked on
Later. This step adds them on top of step 5, before the UI (step 6)
draws them. Rule 9 applies: a rule not written here behaves as the
extract shows.

## The model

- **Kinds:** `service | managed | cron | function`. A cron and a
  function are the same thing, a *run-to-completion* tile, split by
  trigger. Kind stays the user-facing word (matches v0, the stack file
  and the CLI).
- **Per-kind whitelist (B26):** cron carries `command` and `schedule`,
  refuses `port`, `domains`, `replicas > 1`, `healthcheck`; function
  carries `command` and `trigger`, refuses the same plus `schedule`.
  Error strings verbatim from `tilelifecycle.md` ("schedule applies to
  cron tiles only", "run_on_deploy applies to function tiles only",
  "a <kind> has no long-running container to restart; use run instead").
- **Columns on `tiles`:** `schedule TEXT` (cron expr, `CRON_TZ=` prefix
  allowed, validated with `cron.ParseStandard` at save), `trigger TEXT`
  (`manual | on_deploy`, function only), `paused INTEGER` (cron only,
  stored intent, the DECIDE from tilelifecycle.md resolved: a column, not
  a status). No `last_run_at`: last run is a query over `runs`.
- **`runs` table** (own leaf): `id, tile_id, release_id, trigger
  (schedule|manual|deploy), started_at, finished_at, exit_code, status
  (queued|running|ok|failed|cancelled)`. Retention: keep the last 50 per
  tile, prune on insert.
- **Run log** is a file, `$DATA_DIR/runs/<tile_id>/<run_id>.log`, written
  by the worker from the container's log stream while the run goes, kept
  as the last 1 MiB (tail cap, decided 2026-09-24; v0 kept 4 KiB in a
  column). Deleted with its run row by the prune and by tile delete.
  `Logs`/`FollowLogs` with a run id read the file, never the container.
  Log storage as a whole is revisited later.
- **Deploy of a cron/function:** `flow/deploy` builds and tags the image,
  writes the release tile row, starts **no** long-running container.
  Function with `trigger=on_deploy`: deploy ends by queueing one run.
  Cron: deploy ends by reloading the schedule table.
- **A run** is a job (`kind = run`, lock set `tile`) that starts one
  container from the release image (`docker run --rm` semantics through
  the wrapper: same networks, params, volumes as a service tile), waits
  for exit, records exit code and duration in the run row, streams logs
  like a service (`Logs`/`FollowLogs` take a run id). Timeout from
  `timeout_minutes`, no cap (the extract's note). A run that would overlap
  a running one for the same tile is superseded by the lock set, not
  skipped silently: the run row is written `cancelled` with reason
  "previous run still going".
- **Container guard:** Stop on a cron = set `paused`; Stop on a function
  = refused ("nothing to stop"); Restart refused for both; StopRun cancels
  the job.
- **Scheduler:** one `flow/schedule` entry per unpaused cron tile in a
  deployed release, rebuilt on boot and after any write that touches
  `schedule`, `paused`, or a promote (the `CronReload` effect from
  tile-crud.md). A tick enqueues a run and returns.

## Tasks

1. **Schema + `leaf/tile`.** Kinds, three columns, whitelist rows, the
   refusal strings. Migration edits in place (still pre-install).
   Done when: B26 tests cover both kinds.

2. **`leaf/run`.** Rows, `Start(tile, release, trigger)`, `Finish(id,
   exit, status)`, `Last(tile)`, `Next(tile)` (from the expr, pure),
   `List(tile, limit)`, prune.
   Done when: round-trip and prune tests.

3. **`flow/run`.** `Run(tileID, trigger)`: run row first, then the job;
   the worker starts the container, waits, finishes the row. Uses
   `flow/deploy`'s spec builder, never its own. depguard edge
   `run → deploy`.
   Done when: fake-docker test: ok, non-zero exit, timeout.

4. **`flow/deploy` + `flow/schedule` wiring.** No container for run-to-
   completion kinds; on_deploy queues a run; cron reload after deploy,
   pause, schedule edit, promote, delete.
   Done when: a promote of a stack file with a cron tile registers an
   entry; pausing removes it.

5. **Orchestrator verbs.** `RunTile(id) (Job, Run)`, `PauseTile(id, bool)`,
   `Runs(tile, limit)`, `Run(id)`, `StopRun(id)`, `TileStatus` gains
   `LastRun`, `NextRun`, `Paused`. Add to `docs/rewrite/verbs.md` and the
   job-kinds table.

6. **API + CLI.** `POST /api/.../tiles/:tile/run`, `POST .../pause`,
   `GET .../runs`, `GET .../runs/:id`, `DELETE .../runs/:id` (stop).
   CLI: `stackr tile run`, `stackr tile pause|resume`, `stackr tile runs`,
   `stackr tile logs --run <id>`. `docs/openapi.json` regenerated.
   Stack file grammar: `kind: cron` + `schedule:`, `kind: function` +
   `trigger:`; the promote plan diff shows them.

7. **Done gate.** build/lint/test/templint; PROGRESS.md ticked; stacked
   PR on `rewrite-step-5`.
