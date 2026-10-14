-- Node lifecycle. (1) A deleted env, stack or org no longer drops its volumes:
-- they are marked orphaned and keep the node they lived on, so the orphan
-- sweep can take a last backup. (2) backup_groups: one node-wide backup of
-- every volume on a node. (3) jobs gains the 'skipped' state.

ALTER TABLE volumes ADD COLUMN node_id TEXT NOT NULL DEFAULT '';

DROP TRIGGER orgs_scope_cascade;
DROP TRIGGER stacks_scope_cascade;
DROP TRIGGER environments_scope_cascade;

-- Org and stack volumes keep node_id '' (the controller). Schedules go: the
-- volume facts walk to an owner that no longer exists.
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

CREATE TRIGGER stacks_scope_cascade AFTER DELETE ON stacks BEGIN
    DELETE FROM params            WHERE scope_kind = 'stack_pr' AND scope_id = old.id;
    DELETE FROM backup_schedules  WHERE volume_id IN
        (SELECT id FROM volumes WHERE scope_kind = 'stack' AND scope_id = old.id);
    UPDATE volumes SET orphaned_at = COALESCE(orphaned_at, CURRENT_TIMESTAMP)
        WHERE scope_kind = 'stack' AND scope_id = old.id;
    DELETE FROM positions         WHERE scope_kind = 'stack' AND scope_id = old.id;
    DELETE FROM annotations       WHERE scope_kind = 'stack' AND scope_id = old.id;
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
ALTER TABLE backup_runs ADD COLUMN group_id TEXT REFERENCES backup_groups (id) ON DELETE SET NULL;
CREATE INDEX backup_runs_group ON backup_runs (group_id);

-- SQLite cannot widen a CHECK: rebuild jobs. Nothing references it.
CREATE TABLE jobs_new (
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
);
INSERT INTO jobs_new (id, kind, state, release_id, lock_set, payload, waiting_param, log_path, error, created_at, started_at, finished_at)
    SELECT id, kind, state, release_id, lock_set, payload, waiting_param, log_path, error, created_at, started_at, finished_at FROM jobs;
DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;
CREATE INDEX idx_jobs_state ON jobs (state);
