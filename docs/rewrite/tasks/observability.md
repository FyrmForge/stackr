# Observability: metrics and logs (plan, parked 2026-10-13)

Status: half-refined. Not scheduled; picked up after multi-node.

## Decisions so far

- Store: the Victoria family. VictoriaMetrics (metrics) and VictoriaLogs
  (logs), both Apache 2.0, single-node mode.
- **Optional.** An admin enables "Observability" in the panel. Off by default,
  so a stackr install stays one panel plus its infra containers.
- One container, `stackr-observe`, running both binaries under a tiny
  supervisor: one image, one volume, one upgrade path, like `stackr-registry`.
- Listens on the controller only (docker bridge / localhost). Never public,
  never on the mesh. Workers send through the controller.
- Not part of panel backups. Losing it loses history only.
- Grafana-compatible by design: VictoriaMetrics answers as a Prometheus data
  source; VictoriaLogs has an official Grafana plugin. stackr draws its own
  graphs; Grafana is the user's choice, not bundled.
- Rejected: DuckDB embedded (CGO into the one `stackrd` binary that also runs
  on worker hosts; metrics only), Prometheus + Loki (two heavier servers),
  OpenObserve / GreptimeDB (single product, heavier and younger), SQLite
  `metrics.db` (not a time-series store).

## Already built (unmerged)

Branch `worktree-agent-a215f6f01864b8b52` (3 commits on 3797376):

- Worker `GET /v1/metrics`: host CPU/memory/disk and per tile container CPU,
  memory (minus page cache), block IO, network, read from cgroup v2 and
  `/proc` (no Docker stats API; cgroup v1 hosts report none). The controller
  runs the same code in-process via read-only host mounts of `/sys/fs/cgroup`
  and `/proc` into the panel container.
- Controller sampler every 30 s, 8 nodes in parallel, counters turned into
  rates (a counter reset drops one point).
- Series keys: `node:<id>`, `ctr:<tile>/<container>`, `tile:<tile>`,
  `link:<a>|<b>` (rtt_ms, loss from the mesh pings).
- `metrics.Store` interface (write batch, query range with step) and an
  in-memory 24 h implementation (lost on restart).
- Read API: `GET /api/v1/admin/nodes/:node/metrics`,
  `GET /api/v1/admin/nodes/:node/link-metrics?peer=`,
  `GET /api/v1/orgs/.../tiles/:tile/metrics`, `?range=` up to 720h, `?step=`.

Decide on pickup: merge it as the "observability off" baseline (24 h in
memory, live graphs, no history across restarts), or rebase it straight onto
the Victoria store.

## Metrics

- Writes: the sampler batches points to VictoriaMetrics' import API
  (Prometheus text or JSON lines) every 30 s.
- Reads: `query_range` behind the existing `metrics.Store` interface; the
  in-memory store stays as the fallback when observability is off.
- Retention: 30 days at full resolution (`-retentionPeriod=30d`); OSS has no
  downsampling and doesn't need it at this size. Make it a setting.
- Labels: node, tile, container, env, stack, org. Keep org on every series so
  per-org queries can be enforced.
- UI: graphs on the tile drawer (CPU, memory, network, IO), node drawer
  (CPU, memory, disk), and a link drawer opened by clicking any map link
  (RTT and loss over time, endpoints, last handshake).

## Logs

- Today: live only, from `docker logs`; gone when Docker rotates or a
  container is recreated. No search across replicas or nodes.
- Ship: each node (worker, and the controller in-process) follows its tile
  containers' logs through its local Docker API and pushes batches to the
  controller over the tunnel; the controller forwards to VictoriaLogs'
  JSON-lines ingest. Fields: tile, container, replica, env, stack, org, node,
  stream (stdout/stderr), deploy id.
- Back-pressure: a bounded buffer per node; when the controller or
  VictoriaLogs is down, drop oldest and count drops (shown in the UI).
- Retention: 7 days default, a setting.
- UI: the Logs tab gains search and a time range over history (LogsQL under
  the hood); live tail stays as today.
- Panel and stackr's own logs: optional second stream.

## Security

- Per-org isolation: every query goes through the panel, which adds the org
  filter; the stores themselves are never exposed.
- Logs can contain secrets apps print. Treat log storage like app data:
  per-org access, retention limits, a "purge env logs" action.
- `stackr-observe` gets no mesh exposure and no public port; Grafana access
  goes through an admin-created, read-only path if and when added.

## Open questions

1. Grafana access: none, a documented port for admins, or a proxied
   read-only endpoint in the panel?
2. Traces (VictoriaTraces): skip until an app actually sends OTLP.
3. Alerting (vmalert): later, together with notifier connectors.
4. Disk budget: cap and alert when the observe volume nears a limit.
5. Multi-controller / panel standby (later): where observe data lives then.

## Decisions 2026-10-19

- Step 1 (live graphs, in-memory 24 h) ships first as its own 0.8.x release.
- Charts: uPlot, only inside `ui/ts/metric-chart.ts`, so the library can be swapped.
- Chart data is rendered into the page; new points stream over the existing
  SSE stream as appends (no refetch, no replay). The element never fetches.
- SSE stays for all one-way live updates; websockets only where the browser
  talks back (terminal, port forward).
- stackr serves its own `/metrics` (Prometheus format, `prometheus/client_golang`),
  admin-only or internal; stackr-observe scrapes it. Start: Go runtime, HTTP
  request counts and latency, deploy and build counts and durations. (2026-10-21)
- Per-tile traffic: Caddy's built-in per-host metrics (one flag in the JSON
  config), scraped from each ingress node over the tunnel: requests/s, status
  codes, latency, shown next to CPU, memory, IO and network (step 2). Caddy
  access logs ship with container logs into VictoriaLogs as the tile's
  searchable HTTP log (step 3). (2026-10-21)
- Grafana: no access for now; stackr draws its own graphs. A read-only path
  through the panel (org filter applied) only when someone asks. (2026-10-21)
- Disk budget: logs capped by size and age, metrics by age, VictoriaMetrics
  stops writing below a free-space floor; all are settings (defaults: logs 7d
  and 10 GB, metrics 30d). The Observability settings page shows disk used by
  metrics and by logs against their caps, mirrored in the CLI; the panel
  alerts when either nears its cap. (2026-10-21)

## Step 2 plan (approved 2026-10-21)

Scope: metrics history (VictoriaMetrics), stackr's own `/metrics`, Caddy
traffic per tile, disk settings and usage. Logs are step 3.

1. Container `stackr-observe`: its own image
   `ghcr.io/fyrmforge/stackr-observe:<stackr version>` (darthvader
   2026-10-21), the official Victoria release binaries baked in plus a tiny
   supervisor; release.yml builds and pushes it next to the stackr image. Volume
   `stackr-observe-data`, listens on 127.0.0.1 only (the panel is on the
   host network). Spec in `internal/installspec/observe.go`; ensure/remove
   in `internal/service/internal/flow/upgrade/` next to `EnsureRegistry`,
   with an explicit `Pull`. VictoriaLogs ships in the image now, started in
   step 3.
2. Settings (`leaf/settings/catalogue.go`, Flat): `observe_enabled` (off),
   `observe_metrics_retention` (30d), `observe_min_free` (free-space floor).
   `SetSettings` (`internal/service/admin.go`) ensures or stops the
   container on change; boot does the same next to `EnsureRegistry`.
   Turning off stops the container and keeps the volume.
3. Store: a `metrics.Victoria` implementing `metrics.Store` (import API
   writes, `query_range` reads). The sampler writes to memory always and
   to Victoria when on; reads past 24h go to Victoria, and fall back to
   memory if it is down. Labels: key plus node/org/stack/env/tile.
   `MetricsKeep` follows the retention; range picker gains 7d and 30d when on.
4. Caddy traffic: `metrics.per_host` on the http app
   (`leaf/domain/caddy.go`). `metricsTick` reads each ingress Caddy's
   `/metrics` (controller socket, workers via `/v1/caddy`), maps host to
   tile, writes req/s, 4xx/s, 5xx/s, avg latency. Tile drawer gets a
   Traffic chart pair; works with observe off too (24h memory).
5. stackr `/metrics`: `prometheus/client_golang` made direct (already
   indirect). Go runtime, HTTP count and latency, deploy/build count and
   duration. Route `GET /metrics`, admin API key or session, `admin.read`.
   The sampler also writes these into the store, so no scrape config.
6. Disk: Observability tab on the admin settings drawer: on/off, the knobs,
   data size (VictoriaMetrics' own `vm_data_size_bytes`) against free disk.
   CLI `stackr admin observe` mirrors it. Alert `observe.disk` once per
   crossing when free space nears the floor, via `internal/service/alerts.go`.
7. WireGuard per link: each node reads its own `wg show <iface> dump`
   (per peer rx/tx bytes, latest handshake) in `/v1/metrics`; the sampler
   writes `link:<a>|<b>` rx_bps, tx_bps, handshake_age_s next to rtt/loss.
   Tunnel drawer gains Traffic and Handshake charts.

## Step 3 decisions (2026-10-21)

- Collection: the controller pulls. It follows each managed container's
  logs (and the proxies') through the Docker API it already reaches on every
  node over the mTLS worker API, resuming from the last shipped line, and
  writes them into VictoriaLogs' unix socket. Docker's own log file is the
  buffer; nothing new on workers. VictoriaLogs never goes on the mesh: it
  has no auth, and a host-network tile on a worker sends from that worker's
  mesh address. Ceiling: one stream per container; switch to node push if
  stackr ever runs thousands. (Changed from node push, 2026-10-21.)
- Access log fields are admin settings (stackr is self-hosted; the operator
  decides). Defaults: keep time, method, domain, path, status, duration,
  bytes, user agent, client IP; drop query string, cookies, Authorization
  and other headers. Settings turn query string, client IP and headers on or
  off. Caddy strips them before the line is written.
- Every log line carries org, stack, env, tile (ids to filter, names to
  read), tile kind, node, container, stream, source (app, access, run,
  build), run id, deploy id. One log viewer with a scope (org, stack, env,
  tile) and filters; the panel always adds the org filter.
- Runs are the unit for cron and function tiles (Lambda style): run
  containers stay out of the tile graphs; each run records duration, status,
  peak memory and CPU. How: `observability-step3-plan.md`.
