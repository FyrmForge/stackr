# Observability step 3: logs, run metrics, small fixes (plan, 2026-10-21)

Status: draft for darthvader's review. Each task below is sized for one
Sonnet helper in its own worktree off master. Decisions live in
`observability.md` ("Step 3 decisions"); this file is the how.

## Rules for every task

- Read `AGENTS.md`, `CLAUDE.md`, `docs/rewrite/tasks/observability.md` first.
- Never hand-edit generated files: `*_templ.go` (`templ generate`),
  `ui/static/js/elements/*`, `output.css`, `docs/openapi.json`
  (`go run ./cmd/stackrd --dump-openapi > docs/openapi.json`),
  `staticmanifest.go` (`hamr gen static`).
- UI copy: plain, terse, no em dashes. Panel first, CLI mirrors it.
- Done = `make lint`, `make templint`, `go test ./...` (and `cd ui && npm test`
  if TS changed) pass, plus the task's own check.
- One-line commits in repo style, files by name, no `-a`, no AI attribution.
  Never push. Never print secrets. Never touch 192.168.1.99.

## Order

```
Wave 1 (parallel):  F1  F2  F3  L1  R1
Wave 2 (parallel):  L2  L3  L4          (need L1)
Wave 3:             L5                  (needs L2-L4)
Wave 4:             L6  R2              (need L5 / R1)
Then: one rig pass (Opus helper), then release.
```

## Small fixes

### F1. Org slugs that collide with panel routes

- Problem: an org slugged `metrics`, `admin` or `login` loses its home URL
  to a panel route. `slug.Reserved` (`internal/service/internal/slug/slug.go`)
  covers ref keywords only.
- Do: add a `RouteReserved(s)` list (every top-level path segment the panel
  and API register: `metrics`, `admin`, `login`, `api`, `hooks`, `join`,
  `-`, `static`, plus any other found by walking `srv.Echo().Routes()` in a
  test) and refuse it where an org is created or renamed.
- Check: a test that walks the registered routes and asserts every first
  segment is in the list (so a new route can't silently collide again), and
  a test that creating org `metrics` is refused.

### F2. CLI for tunnel metrics

- Problem: tunnel (link) metrics have an API
  (`GET /api/v1/admin/nodes/:node/link-metrics?peer=`) but no CLI.
- Do: `stackr node link-metrics <node> <peer>` mirroring `stackr node
  metrics` (`cmd/stackr/metrics.go`), measures rtt_ms, loss, rx_bps, tx_bps,
  handshake_age_s.
- Check: CLI coverage test passes; a cmd test with a fake server like the
  existing node metrics test.

### F3. Hide charts that can never have data

- Problem: tiles without a domain show an empty Requests and Response time
  chart; tunnels between nodes that only route through another node show an
  empty Traffic chart.
- Do: the view gets a bool from the service (tile has a domain; link is
  direct) and the templ skips those charts
  (`internal/ui/drawer/tile/tile.templ` Metrics,
  `internal/ui/cluster/` tunnel body).
- Check: render tests for both cases.

## Logs

### L1. VictoriaLogs on, settings, disk

- Supervisor (`cmd/stackr-observe/main.go`) already starts VictoriaLogs with
  `-logs`. Pass it when `observe_logs_enabled` is on. VictoriaLogs listens
  on a second unix socket next to `metrics.sock` (`logs.sock`).
- Knobs (`leaf/settings/catalogue.go`, Flat): `observe_logs_enabled` (true),
  `observe_logs_retention` (days, 7), `observe_logs_max_size` (GB, 10) →
  `-retentionPeriod`, `-retention.maxDiskSpaceUsageBytes`. Spec hash change
  recreates the container (existing path in `flow/upgrade/observe.go`).
- Observability tab and `stackr admin observe`: logs on/off, both knobs,
  logs data size (VictoriaLogs' own data-size metric from its `/metrics`).
  `observe.disk` alert counts both stores.
- Check: installspec test for the flags; the tab renders logs size.

### L2. Log store

- `internal/service/internal/logs/` (new): `Store` interface with
  `Write(ctx, []Line)` and `Query(ctx, Filter, from, to, limit) ([]Line, error)`,
  and `Victoria` implementing it over the socket:
  write `POST /insert/jsonline` with `_stream_fields=org_id,stack_id,env_id,tile_id,kind,source`,
  `_time_field=time`, `_msg_field=msg`; read `GET /select/logsql/query`.
- `Line` fields: time, msg, org_id/org, stack_id/stack, env_id/env,
  tile_id/tile, kind (service|cron|function|...), node, container, stream
  (stdout|stderr), source (app|access|run|build), run_id, deploy_id, plus
  free `http.*` fields for access lines. Ids are filtered on; names are for
  display.
- `Filter`: org (required, never empty, enforced in the service), optional
  stack, env, tile, kind, source, stream, run, text. Build LogsQL from it
  with proper quoting; unit test that user text can't break out of the
  quoted phrase.
- Check: tests against an httptest fake, like `metrics/victoria_test.go`.

### L3. Collector (container and proxy logs)

- Decided: the controller pulls. It follows each managed container's logs
  through the Docker API it already reaches on every node (`DockerRouter`,
  the worker proxies Docker at `/` over mTLS). Nothing new on workers,
  VictoriaLogs stays off the mesh.
- `internal/service/logship.go` (new): every 30 s (same tick style as
  `metricsTick`) list managed containers per node; start a follower per
  container not yet followed (`StreamLogs` with `since` = last shipped time
  for that container, kept in memory; first sight = since now); stop
  followers of gone containers. Skip `pause` and `run` (runs are L4).
- Enrich on the controller: tile id label → tile, env, stack, org, kind from
  the DB, cached per tile and dropped on rename/delete.
- Batch: flush every 2 s or 1000 lines to `logs.Store`; a bounded buffer
  (e.g. 10k lines), oldest dropped, drop count kept and shown on the
  Observability tab.
- Off when observe or logs are off: no followers.
- Check: test with `dockerfake` that lines land enriched, a gone container
  stops its follower, a full buffer drops oldest and counts.

### L4. Caddy access logs and run logs

- Access logs: in `leaf/domain/caddy.go` add a `logging` access log per
  server to stdout, JSON, with a `filter` encoder: query string, `Cookie`,
  `Authorization` and other headers removed by default; settings
  `observe_access_query`, `observe_access_ip`, `observe_access_headers`
  (all bools; defaults off, on, off) turn them back on. Regenerate the
  golden testdata.
- The collector (L3) also follows the controller's `stackr-proxy` and each
  ingress node's proxy container, keeps only `http.log.access` lines, maps
  host → tile with the code from `metrics_traffic.go`, writes
  `source=access` with `http.method`, `http.path`, `http.status`,
  `http.duration_ms`, `http.bytes`, `http.ua`, `http.ip`.
- Run logs: `RunOnce` (`leaf/tile/world.go`) already copies each line into
  the run's file. Also hand each line to the log shipper with
  `source=run`, `run_id`. The file stays for the runs tab.
- Check: caddy golden test shows the filter; a test that an access line maps
  to its tile; a run test that its lines reach the store with the run id.

### L5. Log viewer

- One templ component + handler: scope (org, stack, env, tile), filters
  (kind, tile, source, stdout/stderr, run, time range 15m/1h/6h/24h/7d,
  text). Results newest first, 500 lines, "older" button. Live tail stays as
  today on the tile's Logs tab; a "History" toggle switches to the viewer.
- Mounted on: env drawer (new Logs tab), tile Logs tab (scope tile).
  Org filter always added server side from the session's scope.
- API `GET /api/v1/orgs/.../logs?scope&filters` and CLI
  `stackr logs search [--env --tile --kind --source --run --since] [text]`.
- Only shown when observe and logs are on.
- Check: handler tests for scope enforcement (org B's lines never come back
  for org A), filter → query mapping.

### L6. Purge env logs

- Admin and env owner action "Delete this env's logs" (VictoriaLogs delete
  API, filtered by env_id). If the pinned VictoriaLogs version has no delete
  API, stop and report instead of building a workaround.

## Run metrics

### R1. Runs measure themselves

- In `RunOnce` (`leaf/tile/world.go`), while the run is live, poll the
  container's one-shot Docker stats every 2 s; keep peak memory and last
  CPU total. (`ponytail:` a run shorter than one poll may record none.)
- Migration: add `peak_mem_bytes` and `cpu_ms` (nullable) to `runs`.
  Stored when the run finishes (`flow/run/run.go` where status is set).
- At finish, write points under `tile:<id>`: `run_s`, `run_failed` (0/1),
  `run_mem_peak`, `run_cpu_s`.
- Runs tab (`ui/drawer/tile`) shows peak memory and CPU next to "took";
  `stackr tile runs` shows them too.
- Check: a dockerfake test that a run's peak and cpu land on the row and as
  points.

### R2. Metrics tab for cron and function tiles

- For `RunToCompletion` kinds the Metrics tab shows Run duration, Peak
  memory and Failed runs (from R1's points) instead of the replica charts.
- Check: render test for a cron tile's metrics tab.
