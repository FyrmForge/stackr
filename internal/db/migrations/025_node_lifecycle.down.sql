UPDATE jobs SET state = 'cancelled' WHERE state = 'skipped';
CREATE TABLE jobs_old (
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
INSERT INTO jobs_old (id, kind, state, release_id, lock_set, payload, waiting_param, log_path, error, created_at, started_at, finished_at)
    SELECT id, kind, state, release_id, lock_set, payload, waiting_param, log_path, error, created_at, started_at, finished_at FROM jobs;
DROP TABLE jobs;
ALTER TABLE jobs_old RENAME TO jobs;
CREATE INDEX idx_jobs_state ON jobs (state);

DROP INDEX backup_runs_group;
ALTER TABLE backup_runs DROP COLUMN group_id;
DROP TABLE backup_groups;

DROP TRIGGER orgs_scope_cascade;
DROP TRIGGER stacks_scope_cascade;
DROP TRIGGER environments_scope_cascade;

CREATE TRIGGER orgs_scope_cascade AFTER DELETE ON orgs BEGIN
    DELETE FROM params            WHERE scope_kind = 'org' AND scope_id = old.id;
    DELETE FROM params            WHERE scope_kind = 'org_pr' AND scope_id = old.id;
    DELETE FROM volumes           WHERE scope_kind = 'org' AND scope_id = old.id;
    DELETE FROM positions         WHERE scope_kind = 'org' AND scope_id = old.id;
    DELETE FROM annotations       WHERE scope_kind = 'org' AND scope_id = old.id;
END;

CREATE TRIGGER stacks_scope_cascade AFTER DELETE ON stacks BEGIN
    DELETE FROM params            WHERE scope_kind = 'stack_pr' AND scope_id = old.id;
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

ALTER TABLE volumes DROP COLUMN node_id;
