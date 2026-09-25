-- Migration B: every table outside the release model. docs/rewrite/schema.md
-- names each table's leaf and what changed against the old schema.

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
);

-- org_config_plans: one row per plan of the org config file; leaf/orgplan.
-- commit_sha, not commit: COMMIT is an SQL keyword.
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
);

CREATE INDEX org_config_plans_org ON org_config_plans (org_id, created_at DESC);

CREATE TABLE org_members (
    id          TEXT      PRIMARY KEY,
    org_id      TEXT      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    user_id     TEXT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role        TEXT      NOT NULL,
    created_at  DATETIME  NOT NULL,
    UNIQUE (org_id, user_id)
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

-- org_id CASCADE, not SET NULL: a key bound to a deleted org must never turn
-- into an unbound one (DECIDE 13).
CREATE TABLE api_keys (
    id          TEXT      PRIMARY KEY,
    user_id     TEXT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    org_id      TEXT      REFERENCES orgs (id) ON DELETE CASCADE,
    name        TEXT      NOT NULL,
    token_hash  TEXT      NOT NULL UNIQUE,
    created_at  DATETIME  NOT NULL
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
    created_at           DATETIME  NOT NULL,
    UNIQUE (org_id, slug)
);

CREATE TABLE tiles (
    id                          TEXT      PRIMARY KEY,
    stack_id                    TEXT      NOT NULL REFERENCES stacks (id) ON DELETE CASCADE,
    environment_id              TEXT      NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    name                        TEXT      NOT NULL,
    slug                        TEXT      NOT NULL,
    kind                        TEXT      NOT NULL CHECK (kind IN ('service', 'image', 'managed', 'cron', 'function', 'slice')),
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
    slice_access                TEXT      NOT NULL, -- consumers: JSON [{from, access}], from a slice tile slug in this env
    created_at                  DATETIME  NOT NULL,
    updated_at                  DATETIME  NOT NULL,
    UNIQUE (environment_id, slug)
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

CREATE TABLE params (
    id          TEXT      PRIMARY KEY,
    scope_kind  TEXT      NOT NULL CHECK (scope_kind IN ('org', 'stack', 'env')),
    scope_id    TEXT      NOT NULL,
    collection  TEXT      NOT NULL,
    name        TEXT      NOT NULL,
    kind        TEXT      NOT NULL CHECK (kind IN ('param', 'secret')),
    value       TEXT      NOT NULL, -- encrypted
    created_at  DATETIME  NOT NULL,
    updated_at  DATETIME  NOT NULL,
    UNIQUE (scope_kind, scope_id, collection, name)
);

CREATE TABLE settings (
    key    TEXT  PRIMARY KEY,
    value  TEXT  NOT NULL
);

-- allow: org:stack:env:tile patterns that may take a slice ([] = the tile's
-- own env); env_pairs: consumer env name -> this stack's env (DECIDE 194).
CREATE TABLE managed_instances (
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

-- provisions: one per slice tile, the database or bucket on the instance,
-- held by its owner cred.
CREATE TABLE provisions (
    id           TEXT      PRIMARY KEY,
    tile_id      TEXT      NOT NULL UNIQUE REFERENCES tiles (id) ON DELETE CASCADE,
    instance_id  TEXT      NOT NULL REFERENCES managed_instances (id) ON DELETE CASCADE,
    db_name      TEXT      NOT NULL,
    db_user      TEXT      NOT NULL,
    db_password  TEXT      NOT NULL, -- encrypted
    public       INTEGER   NOT NULL,
    on_remove    TEXT      NOT NULL,
    created_at   DATETIME  NOT NULL
);

-- bindings: one consumer's own cred on a slice, at read or write.
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

-- A volume holds data: removing its owning instance never removes the row.
-- A managed tile's volume is env-scoped; stack and org scopes are storage
-- shares (DECIDE 194).
CREATE TABLE volumes (
    id           TEXT      PRIMARY KEY,
    scope_kind   TEXT      NOT NULL CHECK (scope_kind IN ('env', 'stack', 'org')),
    scope_id     TEXT      NOT NULL,
    instance_id  TEXT      REFERENCES managed_instances (id) ON DELETE SET NULL,
    slug         TEXT      NOT NULL,
    name         TEXT      NOT NULL,
    max_size_mb  INTEGER   NOT NULL,
    orphaned_at  DATETIME,
    created_at   DATETIME  NOT NULL,
    UNIQUE (scope_kind, scope_id, slug)
);

-- domain_resources: the hosts stackr names tiles under, at instance, org or
-- stack level (REWRITE.md "Domain resources"). The level says which owner id
-- is set. declared: someone asked for the row; the ones stackr makes itself
-- (the root seed, an org's default) are not, and lose to one that is.
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
);

CREATE UNIQUE INDEX idx_domains_host_path ON domains (host, path);

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

CREATE TABLE connectors (
    id          TEXT      PRIMARY KEY,
    org_id      TEXT      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    provider    TEXT      NOT NULL,
    name        TEXT      NOT NULL,
    host        TEXT      NOT NULL,
    config      TEXT      NOT NULL, -- encrypted: holds the App private key
    created_at  DATETIME  NOT NULL,
    UNIQUE (org_id, host)
);

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

-- Runs outlive their volume: an orphan's last archive must stay restorable.
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
);

-- positions: where a card sits on one canvas, shared by everyone who sees
-- it (ui-plan decided 1). scope_kind home carries the viewer's user id: the
-- home canvas is the only one that differs per viewer. A card without a row
-- is laid out by the graph service.
CREATE TABLE positions (
    scope_kind  TEXT     NOT NULL CHECK (scope_kind IN ('home', 'org', 'stack', 'env')),
    scope_id    TEXT     NOT NULL,
    node_id     TEXT     NOT NULL,
    x           INTEGER  NOT NULL,
    y           INTEGER  NOT NULL,
    PRIMARY KEY (scope_kind, scope_id, node_id)
);

-- annotations: notes and boxes drawn as cards on one canvas.
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

CREATE INDEX annotations_scope ON annotations (scope_kind, scope_id);

-- Polymorphic scopes cannot be a foreign key; these triggers are their
-- cascade. SQLite fires them for rows an FK cascade removes as well.
CREATE TRIGGER orgs_scope_cascade AFTER DELETE ON orgs BEGIN
    DELETE FROM params            WHERE scope_kind = 'org' AND scope_id = old.id;
    DELETE FROM volumes           WHERE scope_kind = 'org' AND scope_id = old.id;
    DELETE FROM positions         WHERE scope_kind = 'org' AND scope_id = old.id;
    DELETE FROM annotations       WHERE scope_kind = 'org' AND scope_id = old.id;
END;

CREATE TRIGGER stacks_scope_cascade AFTER DELETE ON stacks BEGIN
    DELETE FROM params            WHERE scope_kind = 'stack' AND scope_id = old.id;
    DELETE FROM volumes           WHERE scope_kind = 'stack' AND scope_id = old.id;
    DELETE FROM positions         WHERE scope_kind = 'stack' AND scope_id = old.id;
    DELETE FROM annotations       WHERE scope_kind = 'stack' AND scope_id = old.id;
END;

CREATE TRIGGER environments_scope_cascade AFTER DELETE ON environments BEGIN
    DELETE FROM params            WHERE scope_kind = 'env' AND scope_id = old.id;
    DELETE FROM volumes           WHERE scope_kind = 'env' AND scope_id = old.id;
    DELETE FROM positions         WHERE scope_kind = 'env' AND scope_id = old.id;
    DELETE FROM annotations       WHERE scope_kind = 'env' AND scope_id = old.id;
END;

-- runs: one row per run of a cron or function tile. The leaf keeps the last
-- 50 per tile; the log is a file under DATA_DIR/runs/<tile_id>/<id>.log.
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
);

CREATE INDEX runs_tile ON runs (tile_id, created_at);
