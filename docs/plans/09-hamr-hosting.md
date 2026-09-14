# Plan: hosting hamr apps on stackr (early nugget)

Status: discussion notes, not approved for implementation. Captured 2026-08-31.

## Goal

Better dev ex for deploying hamr-scaffolded apps onto stackr. A hamr app
already deploys today (scaffolded Dockerfile → git-built service tile); the
gap is everything around the container.

## Decided so far

- **Janitor jobs move out of the app process and onto stackr's scheduler.**
  In-process `pkg/janitor` runs every task in every replica once you scale;
  platform-side crons fix correctness and give per-run status/logs.
- **Invocation: CLI flag on the app binary, same image.** Scaffold gains e.g.
  `./site --run-task <name>` — boots config+db, runs one janitor task via a
  `RunOnce`, exits with status. No HTTP endpoint, no auth surface.
- **Migrations stay in-app at boot.** golang-migrate's pg lock makes
  concurrent boots safe; no function tile needed.
- **Config generation direction (leaning, not final): hamr generates stackr
  config** (`hamr gen stackr` or part of `hamr new`) — emits config matching
  `hamr.toml` `[options]`: postgres → managed tile slice, sqlite → volume at
  `/data` + replicas pinned to 1, storage=s3 → RustFS bucket, env vars wired
  via varrefs. Keeps stackr framework-agnostic.

## Key findings from the stackr dig

- Cron/function tiles in `stackr-compose.yml` take `build:`/`image:`,
  `command` (`sh -c` override), env varrefs, timeouts
  (`internal/stackrd/config/stackconf/stackconf.go`, `config/runpolicy/`).
- **Gap: a tile cannot reference another tile's image.** N janitor crons from
  one repo = N separate build sources of the same Dockerfile, deploying
  independently — a cron can run a different image version than the web tile.
- **Stackr already has the "same image" mechanic**: app-attached scheduled
  jobs (`internal/stackrd/infra/jobs/jobs.go`) with modes `exec` / `image`
  (one-shot from the app's own built image, app env, run history,
  overlap-skip) / `custom`. But they are UI/API only — **not expressible in
  config-as-code**.

## The beginning nugget

Expose app-attached scheduled jobs in `stackr-compose.yml`: a `jobs:` block
under a service tile (name, schedule, command, mode defaulting to `image`,
timeout). Near-zero new runtime code — jobs already run, record, and skip
overlaps; this is stackconf parse/plan/apply surface.

Then hamr's side generates that block from the app's registered janitor tasks.

## Open questions (next discussion)

- **Which CLI owns the generation** — `hamr gen stackr` vs a `stackr` CLI
  command that inspects a hamr repo. Not decided.
- How janitor task names/schedules get from Go code into the generated
  config (a `--list-tasks` flag on the binary? parse? manual?).
- `jobs:` block schema details: overlap policy, per-env overlays, diff/plan
  rendering, canvas representation.
- Later candidates parked: `hamr mock-serve` as a tile in PR envs; SQLite
  backup path (WAL-safe `.backup` / Litestream, not raw volume copy);
  websocket hub is in-process → sticky sessions or replicas=1.
