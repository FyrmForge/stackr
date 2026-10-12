-- Env moves: one durable row per move of an env between nodes. phase is the
-- step to do or in progress, so a crash resumes at it. from_node and to_node
-- are node ids with no FK: a forgotten node must not erase the history.
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
);

CREATE INDEX env_moves_env ON env_moves (env_id, started_at);
