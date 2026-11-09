CREATE TABLE annotations (
    id          TEXT      PRIMARY KEY,
    scope_kind  TEXT      NOT NULL CHECK (scope_kind IN ('home', 'org', 'stack', 'env')),
    scope_id    TEXT      NOT NULL,
    kind        TEXT      NOT NULL CHECK (kind IN ('note', 'box')),
    x           INTEGER   NOT NULL,
    y           INTEGER   NOT NULL,
    w           INTEGER   NOT NULL,
    h           INTEGER   NOT NULL,
    text        TEXT      NOT NULL,
    created_at  DATETIME  NOT NULL
);

CREATE TABLE backup_groups (
    id          TEXT     PRIMARY KEY,
    node_id     TEXT     NOT NULL,  -- no FK: groups outlive a removed node
    node_name   TEXT     NOT NULL,
    label       TEXT     NOT NULL,  -- "node h3, 14 Oct"
    reason      TEXT     NOT NULL CHECK (reason IN ('manual', 'remove')),
    dest_id     TEXT     NOT NULL,
    warning     TEXT     NOT NULL,  -- "" or the local-fallback warning
    status      TEXT     NOT NULL,
    error       TEXT     NOT NULL,
    created_at  DATETIME NOT NULL,
    finished_at DATETIME
);

CREATE TABLE cluster_ca (
    id                   INTEGER   PRIMARY KEY CHECK (id = 1),
    current_fp           TEXT      NOT NULL,
    previous_fp          TEXT      NOT NULL DEFAULT '',
    rollover_started_at  DATETIME,
    rollover_finished_at DATETIME,
    break_glass_at       DATETIME
);

CREATE TABLE "exit_ladders" (
    scope_kind  TEXT     NOT NULL CHECK (scope_kind IN ('org', 'stack', 'env', 'tile', 'node')),
    scope_id    TEXT     NOT NULL,
    position    INTEGER  NOT NULL CHECK (position >= 0),
    target_id     TEXT     NOT NULL,
    PRIMARY KEY (scope_kind, scope_id, position),
    UNIQUE (scope_kind, scope_id, target_id)
);

CREATE TABLE forgotten_nodes (
    id           TEXT      PRIMARY KEY,
    name         TEXT      NOT NULL,
    forgotten_at TIMESTAMP NOT NULL
);

CREATE TABLE idempotency_keys (
    id            TEXT      PRIMARY KEY,
    request_hash  TEXT      NOT NULL,
    status        INTEGER   NOT NULL DEFAULT 0,
    body          TEXT      NOT NULL DEFAULT '',
    created_at    DATETIME  NOT NULL
);

CREATE TABLE images (
    id           TEXT      PRIMARY KEY,
    ref          TEXT      NOT NULL UNIQUE,
    digest       TEXT      NOT NULL,
    built_at     DATETIME,
    last_digest  TEXT      NOT NULL,
    last_tag     TEXT      NOT NULL,
    last_error   TEXT      NOT NULL,
    checked_at   DATETIME,
    created_at   DATETIME  NOT NULL
);

CREATE TABLE ingress_picks (
    scope_kind  TEXT  NOT NULL CHECK (scope_kind IN ('org', 'stack', 'env', 'tile')),
    scope_id    TEXT  NOT NULL,
    placement_id     TEXT  NOT NULL,
    PRIMARY KEY (scope_kind, scope_id)
);

CREATE TABLE join_tokens (
    id             TEXT      PRIMARY KEY,
    hash           TEXT      NOT NULL UNIQUE,
    node_name      TEXT      NOT NULL,
    tags           TEXT      NOT NULL,
    created_by     TEXT      NOT NULL,
    created_at     DATETIME  NOT NULL,
    expires_at     DATETIME  NOT NULL,
    used_at        DATETIME,
    node_id        TEXT      NOT NULL,
    csr            TEXT      NOT NULL,
    wg_public_key  TEXT      NOT NULL,
    facts          TEXT      NOT NULL,
    cert_pem       TEXT      NOT NULL,
    rejected_at    DATETIME
, node_address TEXT NOT NULL DEFAULT '');

CREATE TABLE node_certs (
    serial      TEXT      PRIMARY KEY,
    node_id     TEXT      NOT NULL,
    role        TEXT      NOT NULL CHECK (role IN ('controller', 'worker')),
    not_after   DATETIME  NOT NULL,
    revoked_at  DATETIME,
    created_at  DATETIME  NOT NULL
, issuer TEXT NOT NULL DEFAULT '');

CREATE TABLE nodes (
    id             TEXT      PRIMARY KEY,
    name           TEXT      NOT NULL UNIQUE,
    slug           TEXT      NOT NULL UNIQUE,
    role           TEXT      NOT NULL CHECK (role IN ('controller', 'worker')),
    tags           TEXT      NOT NULL,
    range_cidr     TEXT      NOT NULL UNIQUE,
    wg_public_key  TEXT      NOT NULL,
    wg_address     TEXT      NOT NULL,
    status         TEXT      NOT NULL,
    created_at     DATETIME  NOT NULL
, public_ip TEXT NOT NULL DEFAULT '', address TEXT NOT NULL DEFAULT '');

CREATE TABLE "params" (
    id          TEXT      PRIMARY KEY,
    scope_kind  TEXT      NOT NULL CHECK (scope_kind IN ('org', 'env', 'server', 'tier', 'stack_pr', 'org_pr')),
    scope_id    TEXT      NOT NULL,
    collection  TEXT      NOT NULL,
    name        TEXT      NOT NULL,
    kind        TEXT      NOT NULL CHECK (kind IN ('param', 'secret')),
    value       TEXT      NOT NULL, -- encrypted
    created_at  DATETIME  NOT NULL,
    updated_at  DATETIME  NOT NULL,
    UNIQUE (scope_kind, scope_id, collection, name)
);

CREATE TABLE placements (
    id          TEXT      PRIMARY KEY,
    name        TEXT      NOT NULL UNIQUE,
    node_id     TEXT      NOT NULL DEFAULT '',
    open_to_all INTEGER   NOT NULL DEFAULT 1,
    created_at  DATETIME  NOT NULL
);

CREATE TABLE positions (
    scope_kind  TEXT     NOT NULL CHECK (scope_kind IN ('home', 'org', 'stack', 'env')),
    scope_id    TEXT     NOT NULL,
    node_id     TEXT     NOT NULL,
    x           INTEGER  NOT NULL,
    y           INTEGER  NOT NULL,
    PRIMARY KEY (scope_kind, scope_id, node_id)
);

CREATE TABLE registry_tokens (
    id          TEXT      PRIMARY KEY,
    kind        TEXT      NOT NULL CHECK (kind IN ('node', 'builder', 'user', 'admin')),
    subject_id  TEXT      NOT NULL,
    scope       TEXT      NOT NULL, -- granted access, "repo:actions" per space-separated entry
    created_at  DATETIME  NOT NULL,
    expires_at  DATETIME  NOT NULL
);

CREATE TABLE routes (
    id          TEXT      PRIMARY KEY,
    host        TEXT      NOT NULL UNIQUE,
    mode        TEXT      NOT NULL CHECK (mode IN ('passthrough', 'http', 'https')),
    target      TEXT      NOT NULL,
    insecure    INTEGER   NOT NULL,
    created_at  DATETIME  NOT NULL
);

CREATE TABLE security_events (
    id          TEXT     PRIMARY KEY,
    at          DATETIME NOT NULL,
    action      TEXT     NOT NULL,
    actor_id    TEXT     NOT NULL DEFAULT '', -- user id, '' = nobody (a failed login)
    key_id      TEXT     NOT NULL DEFAULT '', -- the API key behind the act, if any
    target_kind TEXT     NOT NULL DEFAULT '',
    target_id   TEXT     NOT NULL DEFAULT '',
    ip          TEXT     NOT NULL DEFAULT '',
    user_agent  TEXT     NOT NULL DEFAULT '',
    detail      TEXT     NOT NULL DEFAULT ''
);

CREATE TABLE server_config_plans (
    id          TEXT      PRIMARY KEY,
    commit_sha  TEXT      NOT NULL,
    summary     TEXT      NOT NULL,
    plan        TEXT      NOT NULL, -- JSON serverconfig.Plan
    status      TEXT      NOT NULL CHECK (status IN ('pending', 'clean', 'error', 'superseded', 'applied', 'rejected')),
    error       TEXT      NOT NULL,
    created_at  DATETIME  NOT NULL,
    decided_at  DATETIME,
    source      TEXT      NOT NULL CHECK (source IN ('repo', 'local')),
    file        TEXT      NOT NULL,
    ticked      TEXT      NOT NULL DEFAULT '[]',
    confirmed   INTEGER   NOT NULL DEFAULT 0
);

CREATE TABLE sessions (
    id          TEXT     PRIMARY KEY,
    subject_id  TEXT,
    token       TEXT     NOT NULL UNIQUE,
    expires_at  DATETIME NOT NULL,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE settings (
    key    TEXT  PRIMARY KEY,
    value  TEXT  NOT NULL
);

CREATE TABLE users (
    id              TEXT     PRIMARY KEY,
    email           TEXT     NOT NULL UNIQUE,
    password_hash   TEXT     NOT NULL,
    name            TEXT     NOT NULL,
    role            TEXT     NOT NULL CHECK (role IN ('admin', 'user')),
    active          INTEGER  NOT NULL,
    avatar_path     TEXT     NOT NULL,
    theme           TEXT     NOT NULL,
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL
, time_zone TEXT NOT NULL DEFAULT '');

CREATE TABLE orgs (
    id                   TEXT      PRIMARY KEY,
    name                 TEXT      NOT NULL,
    slug                 TEXT      NOT NULL UNIQUE,
    avatar_path          TEXT      NOT NULL,
    env_colors           TEXT      NOT NULL,
    settings             TEXT      NOT NULL,
    setup_done_at        DATETIME,
    -- the setup wizard's branch: 'config' (the file names it) or 'ui'
    setup_mode           TEXT      NOT NULL DEFAULT '',
    created_at           DATETIME  NOT NULL,
    -- the org config file's binding, the same four stacks has; '' = unbound
    config_connector_id  TEXT      NOT NULL,
    config_repo          TEXT      NOT NULL,
    config_branch        TEXT      NOT NULL,
    config_path          TEXT      NOT NULL,
    config_auto          INTEGER   NOT NULL DEFAULT 0
, placement_id TEXT REFERENCES placements (id) ON DELETE RESTRICT);

CREATE TABLE placement_moves (
    id           TEXT      PRIMARY KEY,
    placement_id TEXT      NOT NULL REFERENCES placements (id) ON DELETE CASCADE,
    from_node    TEXT      NOT NULL,
    to_node      TEXT      NOT NULL,
    restore      BOOLEAN   NOT NULL,
    state        TEXT      NOT NULL CHECK (state IN ('running', 'failed', 'done')),
    by           TEXT      NOT NULL,
    key_id       TEXT      NOT NULL,
    started_at   DATETIME  NOT NULL,
    finished_at  DATETIME,
    error        TEXT      NOT NULL
);

CREATE TABLE placement_orgs (
    placement_id TEXT NOT NULL REFERENCES placements (id) ON DELETE CASCADE,
    org_id       TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    PRIMARY KEY (placement_id, org_id)
);

CREATE TABLE registry_images (
    id            TEXT      PRIMARY KEY,
    org_id        TEXT      REFERENCES orgs (id) ON DELETE CASCADE,
    repo          TEXT      NOT NULL,
    tag           TEXT      NOT NULL,
    digest        TEXT      NOT NULL,
    platforms     TEXT      NOT NULL,
    source        TEXT      NOT NULL CHECK (source IN ('built', 'imported', 'system')),
    upstream_ref  TEXT      NOT NULL,
    size          INTEGER   NOT NULL,
    created_by    TEXT      NOT NULL,
    created_at    DATETIME  NOT NULL,
    UNIQUE (repo, digest)
);

CREATE TABLE shares (
    id            TEXT      PRIMARY KEY,
    org_id        TEXT      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    slug          TEXT      NOT NULL,
    kind          TEXT      NOT NULL CHECK (kind IN ('nfs', 'smb')),
    source        TEXT      NOT NULL,
    options       TEXT      NOT NULL,
    user          TEXT      NOT NULL,
    password_ref  TEXT      NOT NULL,
    created_at    DATETIME  NOT NULL,
    UNIQUE (org_id, slug)
);

CREATE TABLE stacks (
    id                   TEXT      PRIMARY KEY,
    org_id               TEXT      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name                 TEXT      NOT NULL,
    slug                 TEXT      NOT NULL,
    description          TEXT      NOT NULL,
    settings             TEXT      NOT NULL,
    config_connector_id  TEXT      NOT NULL,
    config_repo          TEXT      NOT NULL,
    config_branch        TEXT      NOT NULL,
    config_path          TEXT      NOT NULL,
    created_at           DATETIME  NOT NULL, placement_id TEXT REFERENCES placements (id) ON DELETE RESTRICT,
    UNIQUE (org_id, slug)
);

CREATE TABLE tiers (
    id          TEXT      PRIMARY KEY,
    org_id      TEXT      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    slug        TEXT      NOT NULL,
    position    INTEGER   NOT NULL,
    locked      BOOLEAN   NOT NULL DEFAULT 1,
    created_at  DATETIME  NOT NULL,
    UNIQUE (org_id, slug)
);

CREATE TABLE api_keys (
    id          TEXT      PRIMARY KEY,
    user_id     TEXT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    org_id      TEXT      REFERENCES orgs (id) ON DELETE CASCADE,
    name        TEXT      NOT NULL,
    token_hash  TEXT      NOT NULL UNIQUE,
    created_at  DATETIME  NOT NULL
, level TEXT NOT NULL DEFAULT '', stack_id TEXT REFERENCES stacks (id) ON DELETE CASCADE, expires_at DATETIME, last_used_at DATETIME);

CREATE TABLE backup_destinations (
    id           TEXT      PRIMARY KEY,
    org_id       TEXT      REFERENCES orgs (id) ON DELETE CASCADE, -- NULL = admin-global
    kind         TEXT      NOT NULL CHECK (kind IN ('local', 's3')),
    name         TEXT      NOT NULL,
    endpoint     TEXT      NOT NULL,
    region       TEXT      NOT NULL,
    bucket       TEXT      NOT NULL,
    access_key   TEXT      NOT NULL,
    secret_key   TEXT      NOT NULL, -- encrypted
    archive_key  TEXT      NOT NULL, -- encrypted
    shared       INTEGER   NOT NULL,
    created_at   DATETIME  NOT NULL
, approved_endpoint TEXT NOT NULL DEFAULT '', approved_by TEXT NOT NULL DEFAULT '');

CREATE TABLE "connectors" (
    id          TEXT      PRIMARY KEY,
    org_id      TEXT      REFERENCES orgs (id) ON DELETE CASCADE,
    kind    TEXT      NOT NULL,
    name        TEXT      NOT NULL,
    host        TEXT      NOT NULL,
    settings      TEXT      NOT NULL, -- encrypted: holds the App private key
    created_at  DATETIME  NOT NULL,
    share_all   INTEGER   NOT NULL DEFAULT 0, capabilities TEXT NOT NULL DEFAULT '["git"]', checked_at DATETIME, check_error TEXT NOT NULL DEFAULT '', -- server connector: every org may use it
    UNIQUE (org_id, host)
);

CREATE TABLE credentials (
    id          TEXT      PRIMARY KEY,
    org_id      TEXT      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name        TEXT      NOT NULL,
    url         TEXT      NOT NULL,
    username    TEXT      NOT NULL,
    password    TEXT      NOT NULL, -- encrypted
    created_at  DATETIME  NOT NULL,
    UNIQUE (org_id, name)
);

CREATE TABLE domain_resources (
    id                      TEXT      PRIMARY KEY,
    level                   TEXT      NOT NULL CHECK (level IN ('instance', 'org', 'stack')),
    org_id                  TEXT      REFERENCES orgs (id) ON DELETE CASCADE,
    stack_id                TEXT      REFERENCES stacks (id) ON DELETE CASCADE,
    host                    TEXT      NOT NULL UNIQUE,
    include_env_on_default  INTEGER   NOT NULL,
    acme_email              TEXT      NOT NULL,
    declared                INTEGER   NOT NULL,
    created_at              DATETIME  NOT NULL,
    CHECK (
        (level = 'instance' AND org_id IS NULL AND stack_id IS NULL)
        OR (level = 'org' AND org_id IS NOT NULL AND stack_id IS NULL)
        OR (level = 'stack' AND org_id IS NULL AND stack_id IS NOT NULL)
    )
);

CREATE TABLE host_grants (
    id           TEXT      PRIMARY KEY,
    stack_id     TEXT      NOT NULL UNIQUE REFERENCES stacks (id) ON DELETE CASCADE,
    lines        TEXT      NOT NULL,
    approved_by  TEXT      NOT NULL REFERENCES users (id),
    created_at   DATETIME  NOT NULL
);

CREATE TABLE invites (
    id          TEXT      PRIMARY KEY, -- doubles as the invite-link token
    org_id      TEXT      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    email       TEXT      NOT NULL,
    role        TEXT      NOT NULL,
    created_by  TEXT      NOT NULL,
    created_at  DATETIME  NOT NULL,
    expires_at  DATETIME  NOT NULL,
    used_at     DATETIME
);

CREATE TABLE org_config_plans (
    id          TEXT      PRIMARY KEY,
    org_id      TEXT      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    commit_sha  TEXT      NOT NULL,
    summary     TEXT      NOT NULL,
    plan        TEXT      NOT NULL, -- JSON orgconfig.Plan
    status      TEXT      NOT NULL CHECK (status IN ('pending', 'clean', 'error', 'superseded', 'applied', 'rejected')),
    error       TEXT      NOT NULL,
    created_at  DATETIME  NOT NULL,
    decided_at  DATETIME
, ticked TEXT NOT NULL DEFAULT '[]', confirmed INTEGER NOT NULL DEFAULT 0);

CREATE TABLE org_members (
    id          TEXT      PRIMARY KEY,
    org_id      TEXT      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    user_id     TEXT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role        TEXT      NOT NULL,
    created_at  DATETIME  NOT NULL,
    UNIQUE (org_id, user_id)
);

CREATE TABLE releases (
    id          TEXT      PRIMARY KEY,
    stack_id    TEXT      NOT NULL REFERENCES stacks (id) ON DELETE CASCADE,
    number      INTEGER   NOT NULL,
    created_at  DATETIME  NOT NULL,
    created_by  TEXT      NOT NULL, message TEXT NOT NULL DEFAULT '',
    UNIQUE (stack_id, number)
);

CREATE TABLE connector_shares (
    connector_id  TEXT  NOT NULL REFERENCES connectors (id) ON DELETE CASCADE,
    org_id        TEXT  NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    PRIMARY KEY (connector_id, org_id)
);

CREATE TABLE dns_domains (
    id            TEXT      PRIMARY KEY,
    connector_id  TEXT      NOT NULL REFERENCES connectors (id) ON DELETE CASCADE,
    org_id        TEXT      REFERENCES orgs (id) ON DELETE CASCADE,
    name          TEXT      NOT NULL UNIQUE,
    share_all     INTEGER   NOT NULL,
    org_ids       TEXT      NOT NULL,
    created_at    DATETIME  NOT NULL
, resource_id TEXT REFERENCES domain_resources (id) ON DELETE SET NULL, scope TEXT NOT NULL DEFAULT '', records TEXT NOT NULL DEFAULT '[]');

CREATE TABLE environments (
    id           TEXT      PRIMARY KEY,
    stack_id     TEXT      NOT NULL REFERENCES stacks (id) ON DELETE CASCADE,
    name         TEXT      NOT NULL,
    slug         TEXT      NOT NULL,
    type         TEXT      NOT NULL CHECK (type IN ('static', 'ephemeral')),
    base_env_id  TEXT      REFERENCES environments (id) ON DELETE SET NULL,
    settings     TEXT      NOT NULL,
    color        TEXT      NOT NULL,
    position     INTEGER   NOT NULL,
    network      TEXT      NOT NULL,
    release_id   TEXT      REFERENCES releases (id) ON DELETE SET NULL,
    from_kind    TEXT      NOT NULL CHECK (from_kind IN ('branch', 'promote')),
    from_branch  TEXT      NOT NULL,
    auto         INTEGER   NOT NULL,
    created_at   DATETIME  NOT NULL, locked BOOLEAN NOT NULL DEFAULT 1, node_id TEXT NOT NULL DEFAULT '', subnet  TEXT NOT NULL DEFAULT '', placement_id TEXT NOT NULL DEFAULT '',
    UNIQUE (stack_id, slug)
);

CREATE TABLE "jobs" (
    id             TEXT      PRIMARY KEY,
    kind           TEXT      NOT NULL,
    state          TEXT      NOT NULL CHECK (state IN ('queued', 'running', 'waiting', 'done', 'failed', 'superseded', 'cancelled', 'skipped')),
    release_id     TEXT      REFERENCES releases (id) ON DELETE SET NULL,
    lock_set       TEXT      NOT NULL,
    payload        TEXT      NOT NULL,
    waiting_param  TEXT,
    log_path       TEXT      NOT NULL,
    error          TEXT      NOT NULL,
    created_at     DATETIME  NOT NULL,
    started_at     DATETIME,
    finished_at    DATETIME
, actor_id TEXT NOT NULL DEFAULT '', actor_key_id TEXT NOT NULL DEFAULT '');

CREATE TABLE release_tiles (
    id          TEXT  PRIMARY KEY,
    release_id  TEXT  NOT NULL REFERENCES releases (id) ON DELETE CASCADE,
    slug        TEXT  NOT NULL,
    repo        TEXT  NOT NULL,
    branch      TEXT  NOT NULL,
    commit_sha  TEXT  NOT NULL,
    image_id    TEXT  REFERENCES images (id) ON DELETE SET NULL,
    digest      TEXT  NOT NULL,
    UNIQUE (release_id, slug)
);

CREATE TABLE tiles (
    id                          TEXT      PRIMARY KEY,
    stack_id                    TEXT      NOT NULL REFERENCES stacks (id) ON DELETE CASCADE,
    environment_id              TEXT      NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    name                        TEXT      NOT NULL,
    slug                        TEXT      NOT NULL,
    kind                        TEXT      NOT NULL CHECK (kind IN ('service', 'image', 'operated', 'cron', 'function', 'slice')),
    git_url                     TEXT      NOT NULL,
    git_branch                  TEXT      NOT NULL,
    image_ref                   TEXT      NOT NULL,
    dockerfile_path             TEXT      NOT NULL,
    build_context               TEXT      NOT NULL,
    watch_paths                 TEXT      NOT NULL,
    env_json                    TEXT      NOT NULL,
    build_args                  TEXT      NOT NULL,
    volumes                     TEXT      NOT NULL, -- mount lines: volume-slug:/container/path
    command                     TEXT      NOT NULL,
    container_port              INTEGER   NOT NULL,
    published_ports             TEXT      NOT NULL,
    endpoint_protocol           TEXT      NOT NULL,
    health_path                 TEXT      NOT NULL,
    healthcheck_cmd             TEXT      NOT NULL,
    healthcheck_interval_s      INTEGER   NOT NULL,
    healthcheck_timeout_s       INTEGER   NOT NULL,
    healthcheck_retries         INTEGER   NOT NULL,
    healthcheck_start_period_s  INTEGER   NOT NULL,
    cpu_limit                   REAL      NOT NULL,
    mem_limit_mb                INTEGER   NOT NULL,
    user                        TEXT      NOT NULL,
    shm_size_mb                 INTEGER   NOT NULL,
    privileged                  INTEGER   NOT NULL,
    devices                     TEXT      NOT NULL,
    restart_policy              TEXT      NOT NULL,
    depends_on                  TEXT      NOT NULL,
    files                       TEXT      NOT NULL,
    shared_net                  TEXT      NOT NULL,
    replicas                    INTEGER   NOT NULL,
    update_policy               TEXT      NOT NULL CHECK (update_policy IN ('manual', 'auto')),
    tag_policy                  TEXT      NOT NULL,
    schedule                    TEXT      NOT NULL, -- cron: the cron expression (CRON_TZ= allowed)
    trigger                     TEXT      NOT NULL CHECK (trigger IN ('', 'manual', 'on_deploy')), -- function only
    paused                      INTEGER   NOT NULL, -- cron only: stored intent, the schedule is off
    timeout_minutes             INTEGER   NOT NULL, -- cron, function: a run's timeout, no cap
    provision_from              TEXT, -- slice only: <stack>:<env>:<tile> as written, refs and all
    default_access              TEXT      CHECK (default_access IN ('read', 'write')), -- slice only
    on_remove                   TEXT      CHECK (on_remove IN ('keep', 'drop')), -- slice only: the data when the tile goes
    slice_access                TEXT      NOT NULL, -- consumers: JSON [{from, access}], from a slice tile slug in this env
    created_at                  DATETIME  NOT NULL,
    updated_at                  DATETIME  NOT NULL, lan TEXT NOT NULL DEFAULT '', host_network INTEGER NOT NULL DEFAULT 0,
    UNIQUE (environment_id, slug)
);

CREATE TABLE domains (
    id              TEXT      PRIMARY KEY,
    tile_id         TEXT      NOT NULL REFERENCES tiles (id) ON DELETE CASCADE,
    host            TEXT      NOT NULL,
    path            TEXT      NOT NULL,
    container_port  INTEGER   NOT NULL,
    https           INTEGER   NOT NULL,
    force_https     INTEGER   NOT NULL,
    redirect_to     TEXT      NOT NULL,
    auto            INTEGER   NOT NULL,
    -- the resource that named an auto or apex host; NULL for a literal.
    -- A resource that names one cannot be deleted. NO ACTION, not RESTRICT:
    -- RESTRICT fires mid-cascade, so an org or stack delete would fail on
    -- its own resource before the cascade reached the tile's domains.
    resource_id     TEXT      REFERENCES domain_resources (id),
    position        INTEGER   NOT NULL,
    proxy_json      TEXT      NOT NULL,
    raw_caddy       TEXT      NOT NULL, -- admin-only
    created_at      DATETIME  NOT NULL
, records TEXT NOT NULL DEFAULT '[]');

CREATE TABLE env_moves (
    id           TEXT      PRIMARY KEY,
    env_id       TEXT      NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    from_node    TEXT      NOT NULL,
    to_node      TEXT      NOT NULL,
    live         BOOLEAN   NOT NULL,
    phase        TEXT      NOT NULL CHECK (phase IN ('queued', 'syncing', 'stopping', 'copying', 'final', 'starting', 'cleaning', 'done', 'rolled_back')),
    bytes        INTEGER   NOT NULL,
    started_at   DATETIME  NOT NULL,
    finished_at  DATETIME,
    error        TEXT      NOT NULL
, placement_move_id TEXT);

-- job_tiles: one row per id in a job's lock_set (tiles, envs, plans, "*"), kept
-- by the jobs_tiles_* triggers so every write path stays in step. Lets
-- ListTouching use an index instead of json_each over every job.
-- A later migration that rebuilds jobs (new table, copy, drop, rename) must
-- recreate both triggers and rerun the backfill:
--   INSERT OR IGNORE INTO job_tiles (job_id, tile_id)
--   SELECT j.id, v.value FROM jobs j, json_each(j.lock_set) v;
-- the drop cascades job_tiles away and takes the triggers with it.
CREATE TABLE job_tiles (
    job_id   TEXT  NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    tile_id  TEXT  NOT NULL,
    PRIMARY KEY (job_id, tile_id)
);

CREATE TABLE operated_instances (
    id              TEXT      PRIMARY KEY,
    tile_id         TEXT      NOT NULL REFERENCES tiles (id) ON DELETE CASCADE,
    engine          TEXT      NOT NULL,
    allow           TEXT      NOT NULL DEFAULT '[]', -- JSON list
    env_pairs       TEXT      NOT NULL DEFAULT '{}', -- JSON object
    admin_user      TEXT      NOT NULL,
    admin_password  TEXT      NOT NULL, -- encrypted
    endpoint        TEXT      NOT NULL,
    created_at      DATETIME  NOT NULL
);

CREATE TABLE provisions (
    id           TEXT      PRIMARY KEY,
    tile_id      TEXT      NOT NULL UNIQUE REFERENCES tiles (id) ON DELETE CASCADE,
    instance_id  TEXT      NOT NULL REFERENCES operated_instances (id) ON DELETE CASCADE,
    db_name      TEXT      NOT NULL,
    db_user      TEXT      NOT NULL,
    db_password  TEXT      NOT NULL, -- encrypted
    public       INTEGER   NOT NULL,
    created_at   DATETIME  NOT NULL
);

CREATE TABLE runs (
    id           TEXT      PRIMARY KEY,
    tile_id      TEXT      NOT NULL REFERENCES tiles (id) ON DELETE CASCADE,
    release_id   TEXT      REFERENCES releases (id) ON DELETE SET NULL,
    job_id       TEXT      NOT NULL, -- '' when the run never got a job (cancelled at the door)
    trigger      TEXT      NOT NULL CHECK (trigger IN ('schedule', 'manual', 'deploy')),
    status       TEXT      NOT NULL CHECK (status IN ('queued', 'running', 'ok', 'failed', 'cancelled')),
    exit_code    INTEGER,
    reason       TEXT      NOT NULL,
    created_at   DATETIME  NOT NULL,
    started_at   DATETIME,
    finished_at  DATETIME
, peak_mem_bytes INTEGER, cpu_ms INTEGER);

CREATE TABLE tile_vips (
    tile_id  TEXT  PRIMARY KEY REFERENCES tiles (id) ON DELETE CASCADE,
    node_id  TEXT  NOT NULL,
    vip      TEXT  NOT NULL
);

CREATE TABLE volumes (
    id           TEXT      PRIMARY KEY,
    scope_kind   TEXT      NOT NULL CHECK (scope_kind IN ('env', 'stack', 'org')),
    scope_id     TEXT      NOT NULL,
    instance_id  TEXT      REFERENCES operated_instances (id) ON DELETE SET NULL,
    slug         TEXT      NOT NULL,
    name         TEXT      NOT NULL,
    orphaned_at  DATETIME,
    created_at   DATETIME  NOT NULL, node_id TEXT NOT NULL DEFAULT '',
    UNIQUE (scope_kind, scope_id, slug)
);

CREATE TABLE backup_schedules (
    id          TEXT      PRIMARY KEY,
    volume_id   TEXT      NOT NULL REFERENCES volumes (id) ON DELETE CASCADE,
    method      TEXT      NOT NULL,
    dest_id     TEXT      REFERENCES backup_destinations (id) ON DELETE SET NULL, -- NULL = local default
    cron        TEXT      NOT NULL,
    timezone    TEXT      NOT NULL,
    keep        INTEGER   NOT NULL,
    mode        TEXT      NOT NULL,
    created_at  DATETIME  NOT NULL
);

CREATE TABLE bindings (
    id                TEXT      PRIMARY KEY,
    provision_id      TEXT      NOT NULL REFERENCES provisions (id) ON DELETE CASCADE,
    consumer_tile_id  TEXT      NOT NULL REFERENCES tiles (id) ON DELETE CASCADE,
    access            TEXT      NOT NULL CHECK (access IN ('read', 'write')),
    db_user           TEXT      NOT NULL,
    db_password       TEXT      NOT NULL, -- encrypted
    outputs           TEXT      NOT NULL, -- encrypted JSON
    created_at        DATETIME  NOT NULL,
    UNIQUE (provision_id, consumer_tile_id)
);

CREATE TABLE "dns_records" (
    id             TEXT      PRIMARY KEY,
    dns_domain_id  TEXT      REFERENCES dns_domains (id) ON DELETE SET NULL,
    zone           TEXT      NOT NULL,
    name           TEXT      NOT NULL,
    type           TEXT      NOT NULL,
    value          TEXT      NOT NULL,
    ttl            INTEGER   NOT NULL,
    domain_id      TEXT      REFERENCES domains (id) ON DELETE SET NULL,
    created_at     DATETIME  NOT NULL,
    adopted        INTEGER   NOT NULL DEFAULT 0,
    kind           TEXT      NOT NULL DEFAULT 'app',
    put_back_at    DATETIME,
    handed_back_at DATETIME
);

CREATE TABLE backup_runs (
    id           TEXT      PRIMARY KEY,
    kind         TEXT      NOT NULL CHECK (kind IN ('volume', 'panel')),
    volume_id    TEXT      REFERENCES volumes (id) ON DELETE SET NULL,
    schedule_id  TEXT      REFERENCES backup_schedules (id) ON DELETE SET NULL,
    dest_id      TEXT      NOT NULL REFERENCES backup_destinations (id) ON DELETE CASCADE,
    trigger      TEXT      NOT NULL,
    status       TEXT      NOT NULL,
    object_key   TEXT      NOT NULL,
    size_bytes   INTEGER   NOT NULL,
    error        TEXT      NOT NULL,
    created_at   DATETIME  NOT NULL,
    finished_at  DATETIME
, group_id TEXT REFERENCES backup_groups (id) ON DELETE SET NULL, recovery_gen INTEGER NOT NULL DEFAULT 0, master_gen INTEGER NOT NULL DEFAULT 0);

CREATE INDEX annotations_scope ON annotations (scope_kind, scope_id);
CREATE INDEX backup_runs_dest ON backup_runs (dest_id);
CREATE INDEX backup_runs_group ON backup_runs (group_id);
CREATE INDEX backup_runs_kind ON backup_runs (kind);
CREATE INDEX backup_runs_volume ON backup_runs (volume_id);
CREATE INDEX connector_shares_org ON connector_shares (org_id);
CREATE UNIQUE INDEX connectors_org_kind ON connectors (org_id, kind);
CREATE UNIQUE INDEX connectors_server_name ON connectors (name) WHERE org_id IS NULL;
CREATE INDEX dns_domains_connector ON dns_domains (connector_id);
CREATE UNIQUE INDEX dns_records_addr ON dns_records (zone, name, type)
    WHERE kind != 'custom' AND handed_back_at IS NULL;
CREATE UNIQUE INDEX dns_records_custom ON dns_records (zone, name, type, value)
    WHERE kind = 'custom' AND handed_back_at IS NULL;
CREATE UNIQUE INDEX idx_domains_host_path ON domains (host, path);
CREATE INDEX env_moves_env ON env_moves (env_id, started_at);
CREATE INDEX exit_ladders_node ON exit_ladders (target_id);
CREATE INDEX idempotency_keys_created ON idempotency_keys (created_at);
CREATE INDEX ingress_picks_node ON ingress_picks (placement_id);
CREATE INDEX job_tiles_tile ON job_tiles (tile_id, job_id);
CREATE INDEX idx_jobs_finished ON jobs (finished_at);
CREATE INDEX idx_jobs_state_created ON jobs (state, created_at);
CREATE INDEX join_tokens_node ON join_tokens (node_id);
CREATE INDEX node_certs_node ON node_certs (node_id);
CREATE UNIQUE INDEX nodes_one_controller ON nodes (role) WHERE role = 'controller';
-- One WireGuard key per node: a recovering node cannot take a key another
-- node already holds. Pending nodes may have none yet.
CREATE UNIQUE INDEX nodes_wg_public_key ON nodes(wg_public_key) WHERE wg_public_key != '';
CREATE INDEX org_config_plans_org ON org_config_plans (org_id, created_at DESC);
CREATE INDEX placement_moves_placement ON placement_moves (placement_id, started_at);
CREATE INDEX placements_node ON placements (node_id);
CREATE INDEX registry_images_org ON registry_images (org_id);
CREATE INDEX registry_tokens_expires ON registry_tokens (expires_at);
CREATE INDEX registry_tokens_subject ON registry_tokens (subject_id, created_at);
CREATE INDEX release_tiles_image ON release_tiles (image_id);
CREATE INDEX runs_status ON runs (status);
CREATE INDEX runs_tile ON runs (tile_id, created_at);
CREATE INDEX security_events_action ON security_events (action, at);
CREATE INDEX security_events_actor ON security_events (actor_id, at);
CREATE INDEX security_events_at ON security_events (at);
CREATE INDEX security_events_target ON security_events (target_kind, target_id, at);
CREATE INDEX server_config_plans_created ON server_config_plans (created_at DESC);
CREATE INDEX idx_sessions_subject_id ON sessions (subject_id) WHERE subject_id IS NOT NULL;
CREATE INDEX idx_sessions_token ON sessions (token);

CREATE TRIGGER backup_destinations_keep_runs
BEFORE DELETE ON backup_destinations
WHEN EXISTS (SELECT 1 FROM backup_runs WHERE dest_id = OLD.id)
 AND (OLD.org_id IS NULL OR EXISTS (SELECT 1 FROM orgs WHERE id = OLD.org_id))
BEGIN
    SELECT RAISE(ABORT, 'backup destination still has runs');
END;

CREATE TRIGGER environments_scope_cascade AFTER DELETE ON environments BEGIN
    DELETE FROM params            WHERE scope_kind = 'env' AND scope_id = old.id;
    DELETE FROM backup_schedules  WHERE volume_id IN
        (SELECT id FROM volumes WHERE scope_kind = 'env' AND scope_id = old.id);
    UPDATE volumes SET orphaned_at = COALESCE(orphaned_at, CURRENT_TIMESTAMP),
        node_id = CASE WHEN node_id = '' THEN old.node_id ELSE node_id END
        WHERE scope_kind = 'env' AND scope_id = old.id;
    DELETE FROM positions         WHERE scope_kind = 'env' AND scope_id = old.id;
    DELETE FROM annotations       WHERE scope_kind = 'env' AND scope_id = old.id;
END;

CREATE TRIGGER exit_ladders_env_cascade AFTER DELETE ON environments BEGIN
    DELETE FROM exit_ladders WHERE scope_kind = 'env' AND scope_id = old.id;
END;

CREATE TRIGGER exit_ladders_node_cascade AFTER DELETE ON nodes BEGIN
    DELETE FROM exit_ladders WHERE scope_kind = 'node' AND scope_id = old.id;
END;

CREATE TRIGGER exit_ladders_org_cascade AFTER DELETE ON orgs BEGIN
    DELETE FROM exit_ladders WHERE scope_kind = 'org' AND scope_id = old.id;
END;

CREATE TRIGGER exit_ladders_stack_cascade AFTER DELETE ON stacks BEGIN
    DELETE FROM exit_ladders WHERE scope_kind = 'stack' AND scope_id = old.id;
END;

CREATE TRIGGER exit_ladders_tile_cascade AFTER DELETE ON tiles BEGIN
    DELETE FROM exit_ladders WHERE scope_kind = 'tile' AND scope_id = old.id;
END;

CREATE TRIGGER ingress_picks_env_cascade AFTER DELETE ON environments BEGIN
    DELETE FROM ingress_picks WHERE scope_kind = 'env' AND scope_id = old.id;
END;

CREATE TRIGGER ingress_picks_org_cascade AFTER DELETE ON orgs BEGIN
    DELETE FROM ingress_picks WHERE scope_kind = 'org' AND scope_id = old.id;
END;

CREATE TRIGGER ingress_picks_stack_cascade AFTER DELETE ON stacks BEGIN
    DELETE FROM ingress_picks WHERE scope_kind = 'stack' AND scope_id = old.id;
END;

CREATE TRIGGER ingress_picks_tile_cascade AFTER DELETE ON tiles BEGIN
    DELETE FROM ingress_picks WHERE scope_kind = 'tile' AND scope_id = old.id;
END;

CREATE TRIGGER jobs_tiles_insert AFTER INSERT ON jobs
BEGIN
    INSERT OR IGNORE INTO job_tiles (job_id, tile_id)
    SELECT NEW.id, value FROM json_each(NEW.lock_set);
END;

CREATE TRIGGER jobs_tiles_update AFTER UPDATE OF lock_set ON jobs
WHEN OLD.lock_set IS NOT NEW.lock_set
BEGIN
    DELETE FROM job_tiles WHERE job_id = NEW.id;
    INSERT OR IGNORE INTO job_tiles (job_id, tile_id)
    SELECT NEW.id, value FROM json_each(NEW.lock_set);
END;

CREATE TRIGGER orgs_scope_cascade AFTER DELETE ON orgs BEGIN
    DELETE FROM params            WHERE scope_kind = 'org' AND scope_id = old.id;
    DELETE FROM params            WHERE scope_kind = 'org_pr' AND scope_id = old.id;
    DELETE FROM backup_schedules  WHERE volume_id IN
        (SELECT id FROM volumes WHERE scope_kind = 'org' AND scope_id = old.id);
    UPDATE volumes SET orphaned_at = COALESCE(orphaned_at, CURRENT_TIMESTAMP)
        WHERE scope_kind = 'org' AND scope_id = old.id;
    DELETE FROM positions         WHERE scope_kind = 'org' AND scope_id = old.id;
    DELETE FROM annotations       WHERE scope_kind = 'org' AND scope_id = old.id;
END;

CREATE TRIGGER security_events_no_delete BEFORE DELETE ON security_events
WHEN NOT EXISTS (SELECT 1 FROM settings WHERE key = 'security_events_pruning')
BEGIN SELECT RAISE(ABORT, 'security_events is append only'); END;

CREATE TRIGGER security_events_no_update BEFORE UPDATE ON security_events
BEGIN SELECT RAISE(ABORT, 'security_events is append only'); END;

CREATE TRIGGER stacks_scope_cascade AFTER DELETE ON stacks BEGIN
    DELETE FROM params            WHERE scope_kind = 'stack_pr' AND scope_id = old.id;
    DELETE FROM backup_schedules  WHERE volume_id IN
        (SELECT id FROM volumes WHERE scope_kind = 'stack' AND scope_id = old.id);
    UPDATE volumes SET orphaned_at = COALESCE(orphaned_at, CURRENT_TIMESTAMP)
        WHERE scope_kind = 'stack' AND scope_id = old.id;
    DELETE FROM positions         WHERE scope_kind = 'stack' AND scope_id = old.id;
    DELETE FROM annotations       WHERE scope_kind = 'stack' AND scope_id = old.id;
END;

CREATE TRIGGER tiers_scope_cascade AFTER DELETE ON tiers BEGIN
    DELETE FROM params WHERE scope_kind = 'tier' AND scope_id = old.id;
END;

-- Seed: the default placement. A fresh database has no controller node row
-- yet; the boot path gives default its node_id then.
INSERT INTO placements (id, name, node_id, open_to_all, created_at)
VALUES (lower(hex(randomblob(16))), 'default', '', 1, CURRENT_TIMESTAMP);
