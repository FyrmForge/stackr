# 001_initial.up.sql

- **Source:** `internal/stackrd/store/db/migrations/001_initial.up.sql` (551 lines)
- **Commit:** `c2423f0`
- **Taken:** 21 tables v1 still needs, columns verbatim as dumped, plus the UNIQUE constraints that are real rules. Tile columns that belong to another leaf are moved under that leaf, not deleted.
- **Cut:** `config_plans` + `org_config_plans` (plans), `staged_changes` + `env_intended` (drift), `deployments` + `work_items` + `cron_runs` (jobs), `notifications`, `audit_events`, `secret_links` (later), `metrics`, `annotations`, `node_positions`, `graph_groups` (canvas), `servers` + `join_keys` (agents), `storage` + `storage_paths` (shares), `domain_resources` (params), `compose_tiles_archive` (dead), `org_registry_credentials` (built-in-registry).
- **Cuts belong to:** later waves, or nowhere — except five that come back reshaped, see "Plan overrides".

Dumped from a migrated database, so every column below is real, including the
ones an `ALTER` crammed onto one trailing line. Left as dumped; reflowing is
what broke the first squash.

## leaf/org

```sql
CREATE TABLE orgs (
    id           TEXT      PRIMARY KEY,
    name         TEXT      NOT NULL,
    slug         TEXT      NOT NULL UNIQUE,
    created_at   DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    avatar_path  TEXT      NOT NULL DEFAULT '',
    -- Config as code: the repo this org's stack list is declared in. All four
    -- empty means the org is managed by hand (repo.Org.ConfigManaged).
    config_connector_id  TEXT  NOT NULL DEFAULT '',
    config_repo          TEXT  NOT NULL DEFAULT '',
    config_branch        TEXT  NOT NULL DEFAULT '',
    config_path          TEXT  NOT NULL DEFAULT '',
    -- Set when the owner finishes the onboarding wizard; until then /setup is
    -- the flow and settings are locked (middleware.RequireSetupDone).
    setup_done_at        DATETIME
, ui_edits TEXT NOT NULL DEFAULT '', env_colors TEXT NOT NULL DEFAULT '', setup_mode TEXT NOT NULL DEFAULT '', settings TEXT NOT NULL DEFAULT '{}');
-- extract: ui_edits dropped, "drift never promotes" leaves no staged-edit blob.
-- extract: config_* kept but inert, org config files are later.
```

## leaf/stack

```sql
CREATE TABLE stacks (
    id                   TEXT      PRIMARY KEY,
    org_id               TEXT      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name                 TEXT      NOT NULL,
    slug                 TEXT      NOT NULL,
    description          TEXT      NOT NULL DEFAULT '',
    settings             TEXT      NOT NULL DEFAULT '{}',
    created_at           DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    config_connector_id  TEXT      NOT NULL DEFAULT '',
    config_repo          TEXT      NOT NULL DEFAULT '',
    config_branch        TEXT      NOT NULL DEFAULT '',
    config_path          TEXT      NOT NULL DEFAULT '', org_declared INTEGER NOT NULL DEFAULT 0, ui_edits TEXT NOT NULL DEFAULT '', proxy_middlewares TEXT NOT NULL DEFAULT '',
                                   UNIQUE (org_id, slug)
);
-- extract: ui_edits dropped (drift); proxy_middlewares is Traefik-shaped, Caddy now.
-- The config_* four stay: the release pins the commit the config came from.
```

## leaf/user, org_members, invites, api_keys, sessions

```sql
CREATE TABLE users (
    id             TEXT      PRIMARY KEY,
    email          TEXT      NOT NULL UNIQUE,
    password_hash  TEXT      NOT NULL,
    name           TEXT      NOT NULL DEFAULT '',
    role           TEXT      NOT NULL DEFAULT 'user',
    active         INTEGER   NOT NULL DEFAULT 1,
    created_at     DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    notify_prefs   TEXT      NOT NULL DEFAULT '',
    avatar_path    TEXT      NOT NULL DEFAULT '',
    graph_prefs    TEXT      NOT NULL DEFAULT '',
    theme          TEXT      NOT NULL DEFAULT 'system' -- light | dark | system
);
CREATE TABLE org_members (
    org_id      TEXT       NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    user_id     TEXT       NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role        TEXT       NOT NULL DEFAULT 'member', -- owner | member | viewer,
    created_at  TIMESTAMP  NOT NULL,
                           PRIMARY KEY (org_id, user_id)
);
CREATE TABLE invites (
    id          TEXT       PRIMARY KEY, -- doubles AS the invite-link token,
    org_id      TEXT       NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    email       TEXT       NOT NULL DEFAULT '',
    role        TEXT       NOT NULL DEFAULT 'member',
    created_by  TEXT       NOT NULL DEFAULT '',
    created_at  TIMESTAMP  NOT NULL,
    expires_at  TIMESTAMP  NOT NULL,
    used_at     TIMESTAMP
);
-- extract: api_keys also gains org_id in 002, see 002_api_key_org.md
CREATE TABLE api_keys (
    id          TEXT      PRIMARY KEY,
    user_id     TEXT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name        TEXT      NOT NULL,
    token_hash  TEXT      NOT NULL UNIQUE,
    created_at  DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    scopes      TEXT      NOT NULL DEFAULT '[]'
);
CREATE TABLE sessions (
    id          TEXT      PRIMARY KEY,
    subject_id  TEXT,
    token       TEXT      NOT NULL UNIQUE,
    expires_at  DATETIME  NOT NULL,
    created_at  DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- extract: notify_prefs, graph_prefs dropped, notifications and canvas are later.
-- org_members.role has three values; v1 has two, so `viewer` folds or is refused.
```

## leaf/environment

```sql
CREATE TABLE environments (
    id             TEXT      PRIMARY KEY,
    stack_id       TEXT      NOT NULL REFERENCES stacks (id) ON DELETE CASCADE,
    name           TEXT      NOT NULL,
    slug           TEXT      NOT NULL,
    type           TEXT      NOT NULL DEFAULT 'static', -- static | ephemeral,
    base_env_id    TEXT      NOT NULL DEFAULT '', -- env this one was cloned FROM,
    settings       TEXT      NOT NULL DEFAULT '{}',
    created_at     DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    config_branch  TEXT      NOT NULL DEFAULT '',
    apply_policy   TEXT      NOT NULL DEFAULT '', proxy_ip TEXT NOT NULL DEFAULT '', proxy_cidr TEXT NOT NULL DEFAULT '', color TEXT NOT NULL DEFAULT '', position INTEGER NOT NULL DEFAULT 0, network TEXT NOT NULL DEFAULT '',
                             UNIQUE (stack_id, slug)
);
-- extract: apply_policy dropped, replaced by from_kind/from_branch/auto.
-- `position` and `base_env_id` already exist; only the other four are new.
-- `network` is the env's Docker network, which leaf/environment owns.
```

## leaf/tile

The `DATABASE preset` and volume blocks are cut here and reappear under
leaf/managed and leaf/volume below; nothing else is removed.

```sql
CREATE TABLE tiles (
    id                 TEXT      PRIMARY KEY,
    stack_id           TEXT      NOT NULL REFERENCES stacks (id) ON DELETE CASCADE,
    environment_id     TEXT      NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    name               TEXT      NOT NULL,
    slug               TEXT      NOT NULL,
    kind               TEXT      NOT NULL DEFAULT 'service', -- service | cron,
    --                 SOURCE,
    source_type        TEXT      NOT NULL DEFAULT 'git', -- git | image,
    git_url            TEXT      NOT NULL DEFAULT '',
    git_branch         TEXT      NOT NULL DEFAULT 'main',
    image_ref          TEXT      NOT NULL DEFAULT '',
    dockerfile_path    TEXT      NOT NULL DEFAULT 'Dockerfile',
    build_context      TEXT      NOT NULL DEFAULT '.',
    --                 RUNTIME,
    env                TEXT      NOT NULL DEFAULT '', -- KEY=VALUE lines,
    build_args         TEXT      NOT NULL DEFAULT '', -- KEY=VALUE lines,
    volumes            TEXT      NOT NULL DEFAULT '', -- one per line: name-or-hostpath:/container/path,
    container_port     INTEGER   NOT NULL DEFAULT 0, -- PRIMARY port, 0 = none,
    healthcheck_cmd    TEXT      NOT NULL DEFAULT '',
    webhook_token      TEXT      NOT NULL,
    status             TEXT      NOT NULL DEFAULT 'idle', -- idle | running | stopped | error | done,
    cpu_limit          REAL      NOT NULL DEFAULT 0, -- cores, 0 = unlimited,
    mem_limit_mb       INTEGER   NOT NULL DEFAULT 0, -- MB, 0 = unlimited,
    external_port      INTEGER   NOT NULL DEFAULT 0, -- 0 = internal only,
    --                 CRON      kind: schedule, command override, last run outcome,
    cron               TEXT      NOT NULL DEFAULT '',
    command            TEXT      NOT NULL DEFAULT '',
    allow_overlap      INTEGER   NOT NULL DEFAULT 0,
    timeout_minutes    INTEGER   NOT NULL DEFAULT 30,
    last_run_at        DATETIME,
    last_status        TEXT      NOT NULL DEFAULT '',
    last_output        TEXT      NOT NULL DEFAULT '',
    created_at         DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    connector_id       TEXT      NOT NULL DEFAULT '',
    basic_auth_user    TEXT      NOT NULL DEFAULT '',
    basic_auth_password TEXT     NOT NULL DEFAULT '',
    sec_headers        INTEGER   NOT NULL DEFAULT 0,
    published_ports    TEXT      NOT NULL DEFAULT '',
    traefik_override   TEXT      NOT NULL DEFAULT '',
    watch_paths        TEXT      NOT NULL DEFAULT '',
    endpoint_protocol  TEXT      NOT NULL DEFAULT 'http',
    endpoint_port_var  TEXT      NOT NULL DEFAULT '',
    user TEXT NOT NULL DEFAULT '', shm_size_mb INTEGER NOT NULL DEFAULT 0, privileged INTEGER NOT NULL DEFAULT 0, devices TEXT NOT NULL DEFAULT '', restart_policy TEXT NOT NULL DEFAULT '', healthcheck_interval_s INTEGER NOT NULL DEFAULT 0, healthcheck_timeout_s INTEGER NOT NULL DEFAULT 0, healthcheck_retries INTEGER NOT NULL DEFAULT 0, healthcheck_start_period_s INTEGER NOT NULL DEFAULT 0, run_on_deploy INTEGER NOT NULL DEFAULT 0, depends_on TEXT NOT NULL DEFAULT '', files TEXT NOT NULL DEFAULT '', storage TEXT NOT NULL DEFAULT '', update_policy TEXT NOT NULL DEFAULT 'off', image_digest TEXT NOT NULL DEFAULT '', latest_digest TEXT NOT NULL DEFAULT '', wait_for_ci INTEGER NOT NULL DEFAULT 0, shared_net TEXT NOT NULL DEFAULT '', home_node   TEXT    NOT NULL DEFAULT '', replicas    INTEGER NOT NULL DEFAULT 1, node_group  TEXT    NOT NULL DEFAULT '',
                                 UNIQUE (environment_id, slug)
);
-- extract: status dropped, state is derived (container first, last job row second).
-- extract: traefik_override dropped, Caddy; proxy config belongs to leaf/domain.
-- extract: home_node, node_group dropped (agents later); wait_for_ci (cigate later);
--   storage (org shares later). replicas stays, it is v1.
-- update_policy, image_digest, latest_digest are image watch, which is v1.
-- `env` stays as the tile's own key list; values become literals or refs.
```

## leaf/image, leaf/release, leaf/job

No tables in 001. Images lived on `tiles.image_ref` / `image_digest` /
`latest_digest` and on the daemon. `releases`, `release_tiles` and `jobs` are
new; `deployments` and `work_items` are the two ancestors of `jobs`, both cut —
see "Plan overrides".

## leaf/params (old `variables`)

```sql
CREATE TABLE variables (
    owner_kind  TEXT       NOT NULL, -- tile | stack | org,
    owner_id    TEXT       NOT NULL,
    name        TEXT       NOT NULL,
    value       TEXT       NOT NULL, -- encrypted,
    secret      INTEGER    NOT NULL DEFAULT 0,
    created_at  TIMESTAMP  NOT NULL,
    updated_at  TIMESTAMP  NOT NULL,
                           PRIMARY KEY (owner_kind, owner_id, name)
);
-- extract: owner_kind 'tile' dropped, there is no tile scope in the param store.
-- Encrypted value and the composite primary key are what survives.
```

## leaf/settings

```sql
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
```

Install-wide only. Per-org, per-stack and per-env knobs are the
`settings TEXT DEFAULT '{}'` blobs on `orgs`, `stacks` and `environments`.

## leaf/volume

No table in 001. A volume was a `tiles` row with these columns set — this is
the whole of what leaf/volume inherits:

```sql
    attached_tile_id   TEXT      NOT NULL DEFAULT '',
    mount_path         TEXT      NOT NULL DEFAULT '',
    volume_name        TEXT      NOT NULL DEFAULT '',
    scope_kind         TEXT      NOT NULL DEFAULT 'env',
    scope_id           TEXT      NOT NULL DEFAULT '',
    max_size_mb        INTEGER   NOT NULL DEFAULT 0,
```

## leaf/domain

```sql
CREATE TABLE domains (
    id              TEXT      PRIMARY KEY,
    tile_id         TEXT      NOT NULL REFERENCES tiles (id) ON DELETE CASCADE,
    host            TEXT      NOT NULL,
    path            TEXT      NOT NULL DEFAULT '/',
    container_port  INTEGER   NOT NULL,
    https           INTEGER   NOT NULL DEFAULT 1,
    created_at      DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    redirect_to     TEXT      NOT NULL DEFAULT '',
    cert_pem        TEXT      NOT NULL DEFAULT '',
    key_pem         TEXT      NOT NULL DEFAULT '',
    auto            INTEGER   NOT NULL DEFAULT 0,
    position        INTEGER   NOT NULL DEFAULT 0 -- order within a tile, the proxy matches in it
, force_https INTEGER NOT NULL DEFAULT 1, middlewares TEXT NOT NULL DEFAULT '', priority INTEGER NOT NULL DEFAULT 0, rule TEXT NOT NULL DEFAULT '');

CREATE UNIQUE INDEX idx_domains_host_path ON domains (host, path, rule);
-- extract: middlewares, priority, rule are Traefik's matcher vocabulary and need
--   a Caddy equivalent; the (host, path, rule) uniqueness rule survives as-is.
-- `redirect_to` is why `public_url` is "first non-redirect domain".
```

## leaf/credential (registry pull creds)

```sql
CREATE TABLE registries (
    id          TEXT      PRIMARY KEY,
    name        TEXT      NOT NULL UNIQUE,
    url         TEXT      NOT NULL,
    domain      TEXT      NOT NULL DEFAULT '', -- TLS domain routed via Traefik (managed registry),
    username    TEXT      NOT NULL DEFAULT '',
    password    TEXT      NOT NULL DEFAULT '',
    managed     INTEGER   NOT NULL DEFAULT 0, -- 1 = the Stackr-run registry:2,
    created_at  DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- extract: domain, managed dropped, they are the built-in-registry half (later).
--   url/username/password is the pull-credential half v1 keeps.
-- extract: no org_id at all, credentials were install-global. v1 scopes them per
--   org like everything else, so this table needs one.
```

## leaf/connector

```sql
CREATE TABLE connectors (
    id          TEXT       PRIMARY KEY,
    org_id      TEXT       NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    provider    TEXT       NOT NULL,
    name        TEXT       NOT NULL,
    config      TEXT       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMP  NOT NULL
);
```

`config` is the provider blob (GitHub App id, installation id, private key).
v1 is GitHub App only, one per org, resolved from the tile's `git_url` host.

## leaf/managed (instances + provisions)

`managed_resources` is the slice, `provisions` its credentials,
`resource_bindings` who may reach it (`requires_network` + a binding is the
"referencing is not access" rule), `resource_outputs` what
`${{ tile.<slug>.<output> }}` resolves to. None of the four declares an FK.

```sql
CREATE TABLE managed_resources (
    id                TEXT       PRIMARY KEY,
    environment_id    TEXT       NOT NULL,
    provider_tile_id  TEXT       NOT NULL,
    name              TEXT       NOT NULL,
    slug              TEXT       NOT NULL, -- referenced AS ${{ tile.<slug>.<output> }},
    kind              TEXT       NOT NULL, -- postgres | s3,
    status            TEXT       NOT NULL, -- active | orphaned,
    public            INTEGER    NOT NULL DEFAULT 0,
    created_at        TIMESTAMP  NOT NULL,
    updated_at        TIMESTAMP  NOT NULL,
                                 UNIQUE (environment_id, slug)
);
CREATE TABLE "provisions" (
    id                TEXT       PRIMARY KEY,
    instance_tile_id  TEXT       NOT NULL,
    consumer_tile_id  TEXT       NOT NULL DEFAULT '',
    env_id            TEXT       NOT NULL,
    db_name           TEXT       NOT NULL,
    db_user           TEXT       NOT NULL,
    db_password       TEXT       NOT NULL,
    secret_name       TEXT       NOT NULL,
    status            TEXT       NOT NULL DEFAULT 'active',
    created_at        TIMESTAMP  NOT NULL,
    public            INTEGER    NOT NULL DEFAULT 0,
    on_remove         TEXT       NOT NULL DEFAULT '',
    resource_slug     TEXT       NOT NULL DEFAULT '' -- ${{ tile.<slug>.<output> }}
);
CREATE TABLE resource_bindings (
    resource_id       TEXT       NOT NULL,
    consumer_tile_id  TEXT       NOT NULL,
    created_at        TIMESTAMP  NOT NULL,
                                 PRIMARY KEY (resource_id, consumer_tile_id)
);
CREATE TABLE resource_outputs (
    resource_id       TEXT     NOT NULL,
    name              TEXT     NOT NULL, -- DATABASE_URL, PGHOST, S3_BUCKET, ...,
    value             TEXT     NOT NULL, -- encrypted,
    secret            INTEGER  NOT NULL DEFAULT 0,
    requires_network  INTEGER  NOT NULL DEFAULT 0, -- only reachable over the provider's shared net,
                               PRIMARY KEY (resource_id, name)
);
```

The instance tile's own preset, cut from the `tiles` block above:

```sql
    engine             TEXT      NOT NULL DEFAULT '', -- postgres | s3,
    db_name            TEXT      NOT NULL DEFAULT '',
    db_user            TEXT      NOT NULL DEFAULT '',
    db_password        TEXT      NOT NULL DEFAULT '',
```

## leaf/backup (destinations, schedules, runs)

```sql
CREATE TABLE backup_destinations (
    id          TEXT      PRIMARY KEY,
    org_id      TEXT      REFERENCES orgs (id) ON DELETE CASCADE, -- NULL = admin-global,
    name        TEXT      NOT NULL,
    endpoint    TEXT      NOT NULL,
    region      TEXT      NOT NULL DEFAULT '',
    bucket      TEXT      NOT NULL,
    access_key  TEXT      NOT NULL,
    secret_key  TEXT      NOT NULL, -- encrypted at rest,
    created_at  DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP
, shared INTEGER NOT NULL DEFAULT 0);
CREATE TABLE backups (
    id              TEXT      PRIMARY KEY,
    tile_id         TEXT      REFERENCES tiles (id) ON DELETE CASCADE,
    destination_id  TEXT      NOT NULL REFERENCES backup_destinations (id) ON DELETE CASCADE,
    kind            TEXT      NOT NULL DEFAULT 'dump', -- dump | volume | stackr,
    container_mode  TEXT      NOT NULL DEFAULT 'pause', -- pause | stop | live (volume only),
    cron            TEXT      NOT NULL,
    timezone        TEXT      NOT NULL DEFAULT '',
    keep_latest     INTEGER   NOT NULL DEFAULT 7,
    enabled         INTEGER   NOT NULL DEFAULT 1,
    created_at      DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE backup_runs (
    id           TEXT      PRIMARY KEY,
    backup_id    TEXT      NOT NULL REFERENCES backups (id) ON DELETE CASCADE,
    trigger      TEXT      NOT NULL DEFAULT 'schedule', -- schedule | manual | pre-restore,
    status       TEXT      NOT NULL DEFAULT 'running', -- running | done | error,
    object_key   TEXT      NOT NULL DEFAULT '',
    size_bytes   INTEGER   NOT NULL DEFAULT 0,
    error        TEXT      NOT NULL DEFAULT '',
    created_at   DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    finished_at  DATETIME
);
-- extract: destinations are S3-shaped only. v1's local-directory destination
--   needs a kind column, or an empty endpoint by convention.
-- extract: `kind` dropped, it was derived from the tile; the plan makes it
--   `method` on the schedule row and never derives it.
-- extract: backups.tile_id becomes volume_id, the unit of backup is the volume.
--   tile_id was nullable and `kind` had a 'stackr' value: that pairing is how
--   the panel self-backup row was stored. It stays a feature (admin-only,
--   outside stack files) and needs a home that is not a volume schedule.
-- `shared` is the admin-global visibility rule: unshared reads as not found.
-- `object_key` is what a cross-volume restore takes as its input.
```

## Foreign keys

**They are enforced — verified, not assumed.** `cmd/stackrd/main.go:192` calls
`db.ConnectContext(ctx, path)` with no options, where `db` is hamr's
`pkg/db/sqlite`; at the pinned v0.35.0 that package defaults `ForeignKeys: true`
(`sqlite.go:78`) and appends `_pragma=foreign_keys(on)` to the DSN
(`sqlite.go:139-140`). So every declared `ON DELETE CASCADE` actually fires. A
comment in the old vars code claiming "SQLite runs without foreign keys here" is
stale — those tables simply declare no FK.

**Every declared FK has an `ON DELETE` clause, and all of them are `CASCADE`**
(002 adds the one `SET NULL`). There is no bare `REFERENCES` in the file. All
21 of them: `api_keys.user_id`, `backup_destinations.org_id`, `backups.tile_id`
+ `.destination_id`, `backup_runs.backup_id`, `config_plans.stack_id`,
`connectors.org_id`, `deployments.tile_id`, `domains.tile_id`,
`environments.stack_id`, `env_intended.environment_id`, `invites.org_id`,
`org_members.org_id` + `.user_id`, `org_registry_credentials.org_id`,
`stacks.org_id`, `storage.server_id` + `.org_id`, `storage_paths.storage_id`,
`tiles.stack_id` + `.environment_id`.

**Missing cascades — parent ids with no FK at all**, the deletes the old store
walked by hand. The four that matter for v1:

| Table | Dangling column(s) |
|---|---|
| `managed_resources` | `environment_id`, `provider_tile_id` |
| `provisions` | `instance_tile_id`, `consumer_tile_id`, `env_id` |
| `resource_bindings` | `resource_id`, `consumer_tile_id` |
| `resource_outputs` | `resource_id` |

Deleting an environment or a tile left all four behind and nothing noticed.
Declare them in the rewrite. Two more cannot be fixed with an FK:
`variables.owner_id` is polymorphic (`owner_kind`), `sessions.subject_id` is
nullable and may be a user or an api key. Every other FK-less parent id belongs
to a cut table (`staged_changes`, `notifications`, `secret_links`,
`audit_events`, `annotations`, `node_positions`, `graph_groups`,
`domain_resources`, `org_config_plans`, `join_keys`, `compose_tiles_archive`,
and the `ref` string keys on `cron_runs` / `metrics`).

## Plan overrides

| Old | v1 |
|---|---|
| `tiles.status` | gone. State is derived: container first, last job row second, so it cannot drift. |
| `environments.apply_policy`, `.config_branch` | gone. Replaced by `from_kind` (branch \| promote), `from_branch`, `auto`. |
| `environments` | keeps `position` and `base_env_id` (both already exist; `base_env_id` is the PR env's base); gains `release_id` = what this env runs. |
| — | new `releases`: id, stack, number (per stack, `#41`), created_at, created_by. |
| — | new `release_tiles`: release, tile slug, repo, branch, commit, image id. Mapped by **slug**, because dev and prod tiles are different rows. Records the **digest**, never just the tag. |
| `deployments` | gone. A deploy is a job; "what does this tile run" is its last finished deploy job. `jobs.release_id` = what that deploy shipped. |
| `work_items` | gone, folded into `jobs`. Keep its shape: `kind`, `dedupe_key` (enqueue supersedes older queued rows of the same kind+key), `payload` JSON, `status` (queued \| running \| done \| error \| cancelled \| superseded), `step` (resume point), `progress` JSON, `attempts`, three timestamps, and both indexes — `(status, created_at)` for the claim, `(kind, dedupe_key, status)` for the supersede. Add `waiting` for unresolved param refs. |
| `config_plans`, `org_config_plans` | gone. No plan rows; promote is the plan (dry run) and the apply. |
| `variables` | becomes `params`: `scope_kind` (org \| stack \| env — no tile), `scope_id`, `collection`, `name`, `kind` (param \| secret), `value`. Still encrypted; values are literal, never refs. |
| tile volume columns | become the `volumes` table with its own leaf, plus `orphaned_at` (row and data stay; re-adding the same slug re-adopts). Scoped to its environment. |
| `backups` (on a tile) | schedules move onto volumes: `volume_id`, `dest`, `cron`, `keep`, `mode`, and a `method` column from day one (`pause` \| `stop` \| `live` \| `pg_dump`) that is **not** derived from the tile kind. |
| `backup_destinations` | gains the local-directory implementation (every install has one, the default) and a per-destination encryption key. |
| `api_keys` | gains `org_id` in 002. |

## Notes for the builder

- The `INSERT INTO servers ('local', ...)` at the foot of 001 is load-bearing:
  stack creation resolves server `local` by literal id, so a database without
  that row could not create a stack. v1 drops `servers` — drop the lookup with
  it, do not port a fake row.
- `tiles` holds four kinds in one table (service, cron, managed instance,
  volume), told apart by `kind`, `engine != ''` and `volume_name != ''`. After
  the split, service and cron are what is left.
- No `forwards`, `moved` or `cigate` table exists in 001 — those features never
  reached the schema, so there is nothing to cut.
- `domain_resources` (`level`, `owner_id`, `host`, `include_env_on_default`,
  `acme_email`) is cut because the plan spells the base domain as a param
  (`api.${{ params.domains.base }}`). `acme_email` has no home in that spelling
  and wants a settings key — worth one line of confirmation.
- Encrypted at rest, carry over: `variables.value`, `resource_outputs.value`,
  `backup_destinations.secret_key`, `tiles.db_password`,
  `provisions.db_password`. Hashed, shown once: `api_keys.token_hash`.
- Only two index shapes are rules rather than speed: `idx_domains_host_path`
  (unique on host+path+rule) and the `UNIQUE` clauses inside the tables above.
  Every other index in 001 is a per-FK lookup; add them back on demand.

Size: source 551 lines, extract 488 lines.
