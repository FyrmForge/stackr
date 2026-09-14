# Shared Infra Tiles

One shared container (postgres, mariadb, s3, redis, …) serving many services:
stackr provisions a logical slice (database / bucket / keyspace) + creds per
consumer instead of spinning up a container per service. Also covers shared
singletons (grafana, analytics, loki) that need reachability, not slicing.

## Agreed model

### Two categories

- **Provisionable instances** — today postgres, mariadb and s3; the live list
  is the entries in `managedtiles.Engines`
  (`internal/stackrd/infra/managedtiles`) carrying a `Provision` hook, not a
  list kept here. Each attached
  consumer gets its own logical slice + scoped creds. Stackr runs a
  provisioner on attach (CREATE DATABASE/USER with grants scoped to that db;
  minio bucket + access key; redis ACL + key prefix or logical db index).
- **Shared singletons** — grafana, umami, loki, etc. Ordinary service tiles at
  a shared scope. Attach = network + url secret only. No per-product API
  integrations (datasources, sites) — done by hand in the app's own UI.

Provisioning on/off is per-attach, defaulted by kind (configurable later).

### Scope

Tiles get `scope_kind` + `scope_id` — extensible values, not a hardcoded
enum. Today: `env`, `stack`, `org`. Later: `server`, `cluster`, cross-org.

- **env** — a tile on that env's canvas (like db tiles today, plus provisioning).
- **stack** — "Shared" section on the stack page; shared between all envs,
  including PR envs.
- **org** — lives on the org page; org gets a small canvas of infra tiles.
  Referenced by name from stack config, never defined there.

On an env canvas, an attached shared instance renders as a read-only
reference node with an edge, click-through to its real home.

### Networking

One docker network **per shared instance**. Consumers join that network on
attach. Scoped reachability: a PR env container reaches the shared postgres
but nothing else in production. Containers join multiple networks; existing
env networks unchanged.

### Consumption

Attach → provisioner runs → creds published as an env-scoped secret; the
service uses `${{ tile.<slice-key>.DATABASE_URL }}` exactly like a dedicated
db tile (the config key is the slice's slug — the old derived
`<instance>-<db>` pattern is gone).
Switching between a dedicated db tile and a shared provision is just changing
where the url secret comes from — service config unchanged.

Lifecycle: provision created on attach. On detach / service delete the
logical db is kept and marked orphaned; deletion is gated (same policy as
volumes). **Exception: PR env provisions auto-drop when the PR closes.**

**Multi-consumer sharing (same env).** A second tile can attach to an
*existing* logical db instead of creating its own — for a cron job or worker
that operates on the same data as a service. Each consumer is its own
provision row (its own url secret) but they share one physical db + creds.
Same env only (the url secret is env-scoped). `DROP DATABASE` runs only when
the last consumer of that logical db drops — dropping a non-last consumer just
removes that row, so shared data is never lost.

### Config-as-code

(Shipped shape — the TOML `provision_from` sketch this section once held was
never built. YAML, in `stackr-compose.yml`:)

```yaml
shared:
  maindb:
    type: managed
    engine: postgres

base:
  tiles:
    api-db:
      from: maindb          # slice tile; ${{ tile.api-db.DATABASE_URL }}
    api:
      type: service
      env:
        DATABASE_URL: ${{ tile.api-db.DATABASE_URL }}
```

- `from:` takes a dotted address (`instance` | `stack.instance` |
  `org.stack.instance`) — org-scoped instances declare in `stackr-org.yml`.
- PR envs: slice tiles in the PR template provision fresh logical dbs per PR,
  auto-dropped on close.

### Backups

Both levels:

- **Per logical db** — `pg_dump <db>` / `mysqldump <db>` / per-bucket mirror.
  Each provision gets its own schedule + restore that touches only that slice.
- **Full volume** — disaster-recovery layer, existing volume backup machinery.

## Phases

1. **Provisioning core, env scope. ✅ DONE.** Schema (`scope_kind`/`scope_id`
   on tiles, `provisions` table: instance, consumer tile, slice name, secret
   name, status). Postgres provisioner (exec in container). Provision/detach
   UI on the service panel, drop + gated delete on the instance panel. Secret
   publish. Orphan on detach/consumer-delete.
2a. **Stack + org scope — networking + provisioning. ✅ DONE.** Per-instance
   shared network (`stackr-shared-<id8>`); instance joins with its tile alias,
   consumers join on deploy. Connection url uses the tile alias (rename-safe,
   resolves across envs). `scope_kind` selector on the db panel; eligibility
   by env/stack/org. Provision optionally injects an env var into the consumer
   and redeploys (that deploy is what joins the shared net). Shared net torn
   down on instance delete.
2b. **Stack + org scope — reference nodes. ✅ DONE.** Read-only ghost card
   (`KindRef`) + dashed edge on an env canvas for each shared instance living
   in another env that a local service provisions from; click-through opens the
   instance's home. Deferred (optional aggregation views, low value since an
   instance already shows on its home env canvas + as a reference where
   consumed): a "Shared" section on the stack page and an org infra canvas.
3. **Config-as-code + PR envs. ✅ DONE** — shipped as `shared:` + slice tiles
   (`from:`), plan/apply, PR-env auto-provision + auto-drop on close.
4. **More kinds + backups.** redis and mongo provisioners (postgres, s3 and
   mariadb are done); per-logical-db
   backup schedules + restore; singleton attach (network + url secret, no
   provisioner).

## Known issues / follow-ups

- **Traefik joins shared nets.** `proxy.go` attaches Traefik to every
  `stackr-` network on boot, so it lands on `stackr-shared-*` too. Harmless
  (Traefik already bridges all env nets and never routes to a db instance),
  just untidy.
- **Scope→env doesn't reap the shared net.** Resetting a shared instance back
  to env scope (or dropping its last provision) leaves the empty shared net +
  the instance's endpoint on it until the instance is deleted.
