-- Migration B: every table outside the release model. docs/rewrite/schema.md
-- names each table's leaf and what changed against the old schema.

CREATE TABLE orgs (
    id             TEXT      PRIMARY KEY,
    name           TEXT      NOT NULL,
    slug           TEXT      NOT NULL UNIQUE,
    avatar_path    TEXT      NOT NULL,
    env_colors     TEXT      NOT NULL,
    settings       TEXT      NOT NULL,
    setup_done_at  DATETIME,
    created_at     DATETIME  NOT NULL
);

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
    domains              TEXT      NOT NULL, -- JSON: the stack file's domain reservations
    created_at           DATETIME  NOT NULL,
    UNIQUE (org_id, slug)
);

CREATE TABLE tiles (
    id                          TEXT      PRIMARY KEY,
    stack_id                    TEXT      NOT NULL REFERENCES stacks (id) ON DELETE CASCADE,
    environment_id              TEXT      NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    name                        TEXT      NOT NULL,
    slug                        TEXT      NOT NULL,
    kind                        TEXT      NOT NULL CHECK (kind IN ('service', 'image', 'managed', 'cron', 'function')),
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

CREATE TABLE managed_instances (
    id              TEXT      PRIMARY KEY,
    tile_id         TEXT      NOT NULL REFERENCES tiles (id) ON DELETE CASCADE,
    engine          TEXT      NOT NULL,
    scope_kind      TEXT      NOT NULL CHECK (scope_kind IN ('env', 'stack', 'org')),
    scope_id        TEXT      NOT NULL,
    admin_user      TEXT      NOT NULL,
    admin_password  TEXT      NOT NULL, -- encrypted
    endpoint        TEXT      NOT NULL,
    created_at      DATETIME  NOT NULL
);

CREATE TABLE provisions (
    id                TEXT      PRIMARY KEY,
    instance_id       TEXT      NOT NULL REFERENCES managed_instances (id) ON DELETE CASCADE,
    consumer_tile_id  TEXT      REFERENCES tiles (id) ON DELETE SET NULL,
    slug              TEXT      NOT NULL,
    db_name           TEXT      NOT NULL,
    db_user           TEXT      NOT NULL,
    db_password       TEXT      NOT NULL, -- encrypted
    outputs           TEXT      NOT NULL, -- encrypted JSON
    public            INTEGER   NOT NULL,
    on_remove         TEXT      NOT NULL,
    created_at        DATETIME  NOT NULL
);

-- A volume holds data: removing its owning instance never removes the row.
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

-- Polymorphic scopes cannot be a foreign key; these triggers are their
-- cascade. SQLite fires them for rows an FK cascade removes as well.
CREATE TRIGGER orgs_scope_cascade AFTER DELETE ON orgs BEGIN
    DELETE FROM params            WHERE scope_kind = 'org' AND scope_id = old.id;
    DELETE FROM managed_instances WHERE scope_kind = 'org' AND scope_id = old.id;
    DELETE FROM volumes           WHERE scope_kind = 'org' AND scope_id = old.id;
END;

CREATE TRIGGER stacks_scope_cascade AFTER DELETE ON stacks BEGIN
    DELETE FROM params            WHERE scope_kind = 'stack' AND scope_id = old.id;
    DELETE FROM managed_instances WHERE scope_kind = 'stack' AND scope_id = old.id;
    DELETE FROM volumes           WHERE scope_kind = 'stack' AND scope_id = old.id;
END;

CREATE TRIGGER environments_scope_cascade AFTER DELETE ON environments BEGIN
    DELETE FROM params            WHERE scope_kind = 'env' AND scope_id = old.id;
    DELETE FROM managed_instances WHERE scope_kind = 'env' AND scope_id = old.id;
    DELETE FROM volumes           WHERE scope_kind = 'env' AND scope_id = old.id;
END;
