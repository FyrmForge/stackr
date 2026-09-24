-- Migration A: the release model (REWRITE.md "Promote and releases").
-- Forward references to stacks and images are fine at CREATE time; 003
-- creates them.

CREATE TABLE releases (
    id          TEXT      PRIMARY KEY,
    stack_id    TEXT      NOT NULL REFERENCES stacks (id) ON DELETE CASCADE,
    number      INTEGER   NOT NULL,
    created_at  DATETIME  NOT NULL,
    created_by  TEXT      NOT NULL,
    UNIQUE (stack_id, number)
);

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
    created_at   DATETIME  NOT NULL,
    UNIQUE (stack_id, slug)
);

CREATE TABLE jobs (
    id             TEXT      PRIMARY KEY,
    kind           TEXT      NOT NULL,
    state          TEXT      NOT NULL CHECK (state IN ('queued', 'running', 'waiting', 'done', 'failed', 'superseded', 'cancelled')),
    release_id     TEXT      REFERENCES releases (id) ON DELETE SET NULL,
    lock_set       TEXT      NOT NULL,
    payload        TEXT      NOT NULL,
    waiting_param  TEXT,
    log_path       TEXT      NOT NULL,
    error          TEXT      NOT NULL,
    created_at     DATETIME  NOT NULL,
    started_at     DATETIME,
    finished_at    DATETIME
);

CREATE INDEX idx_jobs_state ON jobs (state);
