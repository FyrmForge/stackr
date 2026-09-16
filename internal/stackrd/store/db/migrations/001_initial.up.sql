-- Baseline schema, squashed through 018 (2026-09-14): org -> stack ->
-- environment -> tiles, plus everything around them. Dumped FROM a database
-- migrated to head, never retyped, which is the only way a squash keeps every
-- column an ALTER ever added. Left exactly as dumped, trailing commas in the
-- column comments included: reflowing it is how the first attempt at this
-- moved a comma inside a comment and the whole baseline stopped parsing.
--
-- The previous baseline plus seventeen incremental migrations collapsed into
-- this one. Safe because stackr has no deployment anywhere but the test rig:
-- there is no database at an older version for the chain to walk. The next
-- schema change starts a new 002 on top of this, and the chain only grows
-- again once there is an install whose data has to survive.
--
-- A fresh install starts WITH NO orgs; the first admin creates one FROM the
-- root canvas.

CREATE TABLE api_keys (
    id          TEXT      PRIMARY KEY,
    user_id     TEXT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name        TEXT      NOT NULL,
    token_hash  TEXT      NOT NULL UNIQUE,
    created_at  DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    scopes      TEXT      NOT NULL DEFAULT '[]'
);
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
CREATE TABLE config_plans (
    id          TEXT       PRIMARY KEY,
    stack_id    TEXT       NOT NULL REFERENCES stacks(id) ON DELETE CASCADE,
    commit_sha  TEXT       NOT NULL DEFAULT '',
    summary     TEXT       NOT NULL DEFAULT '',
    plan        TEXT       NOT NULL DEFAULT '', -- JSON stackconf.Plan,
    status      TEXT       NOT NULL DEFAULT 'pending', -- pending | superseded | applied | rejected | error,
    error       TEXT       NOT NULL DEFAULT '',
    created_at  TIMESTAMP  NOT NULL,
    decided_at  TIMESTAMP,
    env_slug    TEXT       NOT NULL DEFAULT ''
);
CREATE TABLE connectors (
    id          TEXT       PRIMARY KEY,
    org_id      TEXT       NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    provider    TEXT       NOT NULL,
    name        TEXT       NOT NULL,
    config      TEXT       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMP  NOT NULL
);
CREATE TABLE cron_runs (
    id           TEXT      PRIMARY KEY,
    ref          TEXT      NOT NULL,
    status       TEXT      NOT NULL, -- running | ok | error | skipped,
    output       TEXT      NOT NULL DEFAULT '',
    started_at   DATETIME  NOT NULL,
    finished_at  DATETIME  -- NULL while the run is still going
, trigger TEXT NOT NULL DEFAULT '', actor   TEXT NOT NULL DEFAULT '');
CREATE TABLE deployments (
    id           TEXT      PRIMARY KEY,
    tile_id      TEXT      NOT NULL REFERENCES tiles (id) ON DELETE CASCADE,
    status       TEXT      NOT NULL DEFAULT 'queued', -- queued | running | done | error | cancelled,
    trigger      TEXT      NOT NULL DEFAULT 'manual', -- manual | webhook | ROLLBACK,
    commit_sha   TEXT      NOT NULL DEFAULT '',
    image_tag    TEXT      NOT NULL DEFAULT '',
    error        TEXT      NOT NULL DEFAULT '',
    created_at   DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at   DATETIME,
    finished_at  DATETIME
);
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
CREATE TABLE managed_resources (
    id                TEXT       PRIMARY KEY,
    environment_id    TEXT       NOT NULL,
    provider_tile_id  TEXT       NOT NULL,
    name              TEXT       NOT NULL,
    slug              TEXT       NOT NULL, -- referenced AS ${{ tile.<slug>.<output> }},
    kind              TEXT       NOT NULL, -- postgres | mysql | mongo | s3 | ...,
    status            TEXT       NOT NULL, -- active | orphaned,
    public            INTEGER    NOT NULL DEFAULT 0,
    created_at        TIMESTAMP  NOT NULL,
    updated_at        TIMESTAMP  NOT NULL,
                                 UNIQUE (environment_id, slug)
);
CREATE TABLE metrics (
    ref        TEXT      NOT NULL, -- "app:<id>" | "db:<id>" | "server:local",
    ts         DATETIME  NOT NULL,
    cpu_pct    REAL      NOT NULL,
    mem_bytes  INTEGER   NOT NULL,
    rx_bps     REAL      NOT NULL DEFAULT 0,
    tx_bps     REAL      NOT NULL DEFAULT 0
);
CREATE TABLE "node_positions" (
    owner_id  TEXT  NOT NULL, -- "env:<stackID>" | "stack:<stackID>" | "org:<orgID>",
    node_id   TEXT  NOT NULL, -- "app:<slug>" | "db:<slug>" | "env:<slug>" | ...,
    x         REAL  NOT NULL,
    y         REAL  NOT NULL,
                    PRIMARY KEY (owner_id, node_id)
);
CREATE TABLE notifications (
    id          TEXT      PRIMARY KEY,
    kind        TEXT      NOT NULL, -- deploy_failed | deploy_done | cron_failed,
    title       TEXT      NOT NULL,
    body        TEXT      NOT NULL DEFAULT '',
    link        TEXT      NOT NULL DEFAULT '',
    read        INTEGER   NOT NULL DEFAULT 0,
    created_at  DATETIME  NOT NULL,
    user_id     TEXT      NOT NULL DEFAULT ''
);
CREATE TABLE org_members (
    org_id      TEXT       NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    user_id     TEXT       NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role        TEXT       NOT NULL DEFAULT 'member', -- owner | member | viewer,
    created_at  TIMESTAMP  NOT NULL,
                           PRIMARY KEY (org_id, user_id)
);
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
CREATE TABLE servers (
    id          TEXT      PRIMARY KEY,
    name        TEXT      NOT NULL,
    kind        TEXT      NOT NULL DEFAULT 'local', -- local | swarm | remote,
    endpoint    TEXT      NOT NULL DEFAULT '',
    settings    TEXT      NOT NULL DEFAULT '{}',
    created_at  DATETIME  NOT NULL
, node_id     TEXT NOT NULL DEFAULT '', hostname    TEXT NOT NULL DEFAULT '', address     TEXT NOT NULL DEFAULT '', role        TEXT NOT NULL DEFAULT '', status      TEXT NOT NULL DEFAULT '', last_seen_at DATETIME);
CREATE TABLE sessions (
    id          TEXT      PRIMARY KEY,
    subject_id  TEXT,
    token       TEXT      NOT NULL UNIQUE,
    expires_at  DATETIME  NOT NULL,
    created_at  DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE settings (
    key    TEXT  PRIMARY KEY,
    value  TEXT  NOT NULL
);
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
CREATE TABLE staged_changes (
    id           TEXT       PRIMARY KEY,
    stack_id     TEXT       NOT NULL,
    env_id       TEXT       NOT NULL,
    tile_slug    TEXT       NOT NULL,
    author_id    TEXT       NOT NULL DEFAULT '',
    author_name  TEXT       NOT NULL DEFAULT '',
    summary      TEXT       NOT NULL DEFAULT '', -- short human label, e.g. "env vars",
    payload      TEXT       NOT NULL, -- JSON {field: value},
    created_at   TIMESTAMP  NOT NULL
);
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
    --                 DATABASE  preset (engine != '' marks a db tile),
    engine             TEXT      NOT NULL DEFAULT '', -- postgres | mysql | mariadb | mongo | redis,
    db_name            TEXT      NOT NULL DEFAULT '',
    db_user            TEXT      NOT NULL DEFAULT '',
    db_password        TEXT      NOT NULL DEFAULT '',
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
    basic_auth_hash    TEXT      NOT NULL DEFAULT '',
    sec_headers        INTEGER   NOT NULL DEFAULT 0,
    published_ports    TEXT      NOT NULL DEFAULT '',
    traefik_override   TEXT      NOT NULL DEFAULT '',
    watch_paths        TEXT      NOT NULL DEFAULT '',
    attached_tile_id   TEXT      NOT NULL DEFAULT '',
    mount_path         TEXT      NOT NULL DEFAULT '',
    volume_name        TEXT      NOT NULL DEFAULT '',
    scope_kind         TEXT      NOT NULL DEFAULT 'env',
    scope_id           TEXT      NOT NULL DEFAULT '',
    endpoint_protocol  TEXT      NOT NULL DEFAULT 'http',
    endpoint_port_var  TEXT      NOT NULL DEFAULT '',
    max_size_mb        INTEGER   NOT NULL DEFAULT 0, user TEXT NOT NULL DEFAULT '', shm_size_mb INTEGER NOT NULL DEFAULT 0, privileged INTEGER NOT NULL DEFAULT 0, devices TEXT NOT NULL DEFAULT '', restart_policy TEXT NOT NULL DEFAULT '', healthcheck_interval_s INTEGER NOT NULL DEFAULT 0, healthcheck_timeout_s INTEGER NOT NULL DEFAULT 0, healthcheck_retries INTEGER NOT NULL DEFAULT 0, healthcheck_start_period_s INTEGER NOT NULL DEFAULT 0, run_on_deploy INTEGER NOT NULL DEFAULT 0, depends_on TEXT NOT NULL DEFAULT '', files TEXT NOT NULL DEFAULT '', storage TEXT NOT NULL DEFAULT '', update_policy TEXT NOT NULL DEFAULT 'off', image_digest TEXT NOT NULL DEFAULT '', latest_digest TEXT NOT NULL DEFAULT '', wait_for_ci INTEGER NOT NULL DEFAULT 0, shared_net TEXT NOT NULL DEFAULT '', home_node   TEXT    NOT NULL DEFAULT '', replicas    INTEGER NOT NULL DEFAULT 1, node_group  TEXT    NOT NULL DEFAULT '',
                                 UNIQUE (environment_id, slug)
);
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
CREATE INDEX idx_backup_destinations_org  ON backup_destinations (org_id);
CREATE INDEX idx_backup_runs_backup       ON backup_runs (backup_id, created_at DESC);
CREATE INDEX idx_backups_tile             ON backups (tile_id);
CREATE INDEX idx_config_plans_stack       ON config_plans(stack_id, created_at DESC);
CREATE INDEX idx_connectors_org           ON connectors(org_id);
CREATE INDEX idx_cron_runs_ref            ON cron_runs (ref, started_at DESC);
CREATE INDEX idx_deployments_tile         ON deployments (tile_id, created_at DESC);
CREATE UNIQUE INDEX idx_domains_host_path        ON domains (host, path, rule);
CREATE INDEX idx_resources_provider       ON managed_resources(provider_tile_id);
CREATE INDEX idx_metrics_ref_ts           ON metrics (ref, ts);
CREATE INDEX idx_notifications_read       ON notifications (read, created_at DESC);
CREATE INDEX idx_notifications_user       ON notifications (user_id, created_at DESC);
CREATE INDEX idx_provisions_consumer      ON provisions(consumer_tile_id);
CREATE INDEX idx_provisions_env           ON provisions(env_id);
CREATE INDEX idx_provisions_instance      ON provisions(instance_tile_id);
CREATE INDEX idx_bindings_consumer        ON resource_bindings(consumer_tile_id);
CREATE INDEX idx_sessions_subject_id      ON sessions (subject_id) WHERE subject_id IS NOT NULL;
CREATE INDEX idx_sessions_token           ON sessions (token);
CREATE INDEX idx_staged_env               ON staged_changes(env_id);
CREATE INDEX idx_staged_stack             ON staged_changes(stack_id);
CREATE INDEX idx_tiles_env                ON tiles (environment_id);
CREATE INDEX idx_tiles_stack              ON tiles (stack_id);
CREATE INDEX idx_users_email              ON users (email);
CREATE TABLE domain_resources (
    id                      TEXT       PRIMARY KEY,
    level                   TEXT       NOT NULL, -- instance | org | stack,
    owner_id                TEXT       NOT NULL, -- server / org / stack id,
    host                    TEXT       NOT NULL UNIQUE,
    include_env_on_default  INTEGER    NOT NULL DEFAULT 0,
    created_at              TIMESTAMP  NOT NULL DEFAULT CURRENT_TIMESTAMP
, declared INTEGER NOT NULL DEFAULT 0, acme_email TEXT NOT NULL DEFAULT '');
CREATE TABLE annotations (
    id          TEXT      PRIMARY KEY,
    owner_id    TEXT      NOT NULL, -- "env:<envID>" | "stack:<stackID>" | "org:<orgID>" | "user:<userID>",
    kind        TEXT      NOT NULL, -- "text" | "box",
    body        TEXT      NOT NULL DEFAULT '',
    x           REAL      NOT NULL,
    y           REAL      NOT NULL,
    w           REAL      NOT NULL DEFAULT 0,
    h           REAL      NOT NULL DEFAULT 0,
    color       TEXT      NOT NULL DEFAULT '',
    created_at  DATETIME  NOT NULL,
    updated_at  DATETIME  NOT NULL
);
CREATE INDEX idx_annotations_owner ON annotations(owner_id);
CREATE TABLE secret_links (
    id              TEXT      PRIMARY KEY,
    kind            TEXT      NOT NULL, -- DROP | share,
    owner_kind      TEXT      NOT NULL, -- matches variables.owner_kind,
    owner_id        TEXT      NOT NULL,
    label           TEXT      NOT NULL DEFAULT '',
    token_hash      TEXT      NOT NULL UNIQUE, -- sha256 of the token IN the URL,
    pass_hash       TEXT      NOT NULL DEFAULT '', -- '' = NO passphrase,
    fields          TEXT      NOT NULL DEFAULT '[]',
    state           TEXT      NOT NULL DEFAULT 'open', -- open | burned | revoked | locked,
    attempts        INTEGER   NOT NULL DEFAULT 0,
    window_minutes  INTEGER   NOT NULL DEFAULT 0, -- share: minutes live after first access,
    expires_at      DATETIME  NOT NULL,
    opened_at       DATETIME,
    created_by      TEXT      NOT NULL,
    created_at      DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_secret_links_owner ON secret_links(owner_kind, owner_id);
CREATE TABLE audit_events (
    actor       TEXT      NOT NULL, -- user email, "api:<key>" OR "share-link:<id>",
    action      TEXT      NOT NULL, -- reveal | copy | edit | SET | DELETE | share,
    owner_kind  TEXT      NOT NULL, -- tile | env | stack | org,
    owner_id    TEXT      NOT NULL,
    name        TEXT      NOT NULL, -- the variable's name,
    created_at  DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_audit_events_owner ON audit_events(owner_kind, owner_id);
CREATE TABLE graph_groups (
    id           TEXT      PRIMARY KEY,
    owner_id     TEXT      NOT NULL, -- "env:<envID>" | "stack:<stackID>" | "org:<orgID>" | "user:<userID>",
    member_keys  TEXT      NOT NULL DEFAULT '[]',
    created_at   DATETIME  NOT NULL,
    updated_at   DATETIME  NOT NULL
);
CREATE INDEX idx_graph_groups_owner ON graph_groups(owner_id);
CREATE TABLE storage (
    id          TEXT       PRIMARY KEY,
    -- Exactly one of server_id / org_id: a server pool or share, or an org's
    -- network share (stackr-org.yml storage:, nfs/smb only).
    server_id   TEXT       REFERENCES servers(id) ON DELETE CASCADE,
    org_id      TEXT       REFERENCES orgs(id) ON DELETE CASCADE,
    name        TEXT       NOT NULL,
    slug        TEXT       NOT NULL,
    backend     TEXT       NOT NULL, -- nfs | smb | local,
    address     TEXT       NOT NULL DEFAULT '', -- host for nfs/smb; '' for local,
    export      TEXT       NOT NULL DEFAULT '', -- /export (nfs) | share (smb) | abs path (local),
    username    TEXT       NOT NULL DEFAULT '', -- smb creds,
    password    TEXT       NOT NULL DEFAULT '', -- encrypted at rest (secrets.Encrypt),
    opts        TEXT       NOT NULL DEFAULT '', -- extra mount opts appended to o=,
    status      TEXT       NOT NULL DEFAULT 'unknown', -- ok | error | unknown (last probe),
    status_msg  TEXT       NOT NULL DEFAULT '',
    created_at  TIMESTAMP  NOT NULL,
    CHECK ((server_id IS NULL) != (org_id IS NULL))
);
CREATE UNIQUE INDEX idx_storage_slug ON storage(server_id, slug) WHERE server_id IS NOT NULL;
CREATE UNIQUE INDEX idx_storage_org_slug ON storage(org_id, slug) WHERE org_id IS NOT NULL;
CREATE TABLE storage_paths (
    id          TEXT       PRIMARY KEY,
    storage_id  TEXT       NOT NULL REFERENCES storage(id) ON DELETE CASCADE,
    name        TEXT       NOT NULL,
    subpath     TEXT       NOT NULL DEFAULT '', -- relative to the share/pool root; '' = root,
    forced_ro   INTEGER    NOT NULL DEFAULT 0, -- every attachment of this path IS read-only,
    created_at  TIMESTAMP  NOT NULL
);
CREATE UNIQUE INDEX idx_storage_paths_name ON storage_paths(storage_id, name);
CREATE TABLE org_config_plans (
    id          TEXT       PRIMARY KEY,
    stack_id    TEXT       NOT NULL, -- the ORG id (see header comment),
    commit_sha  TEXT       NOT NULL DEFAULT '',
    summary     TEXT       NOT NULL DEFAULT '',
    plan        TEXT       NOT NULL DEFAULT '', -- JSON stackconf.Plan,
    status      TEXT       NOT NULL DEFAULT 'pending',
    error       TEXT       NOT NULL DEFAULT '',
    created_at  TIMESTAMP  NOT NULL,
    decided_at  TIMESTAMP,
    env_slug    TEXT       NOT NULL DEFAULT ''
);
CREATE INDEX idx_org_config_plans ON org_config_plans(stack_id, created_at DESC);
CREATE TABLE env_intended (
  environment_id TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
  tile_slug      TEXT NOT NULL,
  key            TEXT NOT NULL,
  value          TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (environment_id, tile_slug, key)
);
CREATE TABLE compose_tiles_archive (
  tile_id        TEXT PRIMARY KEY,
  source_type    TEXT NOT NULL,
  compose_path   TEXT NOT NULL DEFAULT '',
  compose_inline TEXT NOT NULL DEFAULT '',
  archived_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE join_keys (
    key         TEXT      PRIMARY KEY,
    server_id   TEXT      NOT NULL,
    address     TEXT      NOT NULL,
    expires_at  DATETIME  NOT NULL,
    used_at     DATETIME,
    created_at  DATETIME  NOT NULL
);
CREATE TABLE work_items (
    id           TEXT      PRIMARY KEY,
    kind         TEXT      NOT NULL,            -- config.apply, tile.deploy, ...
    -- dedupe_key scopes supersession: enqueueing marks older queued rows with
    -- the same kind and key superseded. The stack id for an apply, so two
    -- rapid pushes do not both apply; the tile id for a deploy, which is what
    -- the engine's SupersedeWaiting already does by hand.
    dedupe_key   TEXT      NOT NULL DEFAULT '',
    payload      TEXT      NOT NULL DEFAULT '', -- JSON, the handler's own shape
    -- queued | running | done | error | cancelled | superseded
    status       TEXT      NOT NULL DEFAULT 'queued',
    step         TEXT      NOT NULL DEFAULT '', -- coarse progress, resume point
    progress     TEXT      NOT NULL DEFAULT '', -- JSON, bytes/total and friends
    error        TEXT      NOT NULL DEFAULT '',
    attempts     INTEGER   NOT NULL DEFAULT 0,
    created_at   DATETIME  NOT NULL,
    started_at   DATETIME,
    finished_at  DATETIME
);
CREATE INDEX idx_work_items_claim ON work_items (status, created_at);
CREATE INDEX idx_work_items_kind  ON work_items (kind, dedupe_key, status);
CREATE TABLE org_registry_credentials (
    id          TEXT      PRIMARY KEY,
    org_id      TEXT      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name        TEXT      NOT NULL,
    -- Hashed like an API key: the plaintext is shown once at creation and is
    -- not recoverable, so a leaked database is not a set of push credentials.
    secret_hash TEXT      NOT NULL UNIQUE,
    -- The visible half of the token, for telling two credentials apart in a
    -- list without revealing either.
    prefix      TEXT      NOT NULL DEFAULT '',
    -- Stackr's own deploys use this one; it is created with the org and cannot
    -- be deleted, or the org's next build has nothing to push with.
    system      INTEGER   NOT NULL DEFAULT 0,
    created_at  DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at DATETIME
);
CREATE INDEX idx_org_registry_credentials_org ON org_registry_credentials (org_id);

-- Stack creation resolves server 'local' BY literal id, so a database
-- WITHOUT this row cannot CREATE a stack at all.
INSERT INTO servers (id, name, kind, endpoint, settings, created_at)
VALUES ('local', 'Local server', 'local', '', '{}', CURRENT_TIMESTAMP);
