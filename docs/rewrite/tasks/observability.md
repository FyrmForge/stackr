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
