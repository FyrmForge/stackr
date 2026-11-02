-- A job remembers who asked for it ('' = the system: a schedule, a push, a watch).
ALTER TABLE jobs ADD COLUMN actor_id TEXT NOT NULL DEFAULT '';

-- Idempotency-Key on the endpoints that queue a job: the key (hashed with its
-- principal and route) maps to the first answer, kept a day. status 0 = the
-- first request is still running.
CREATE TABLE idempotency_keys (
    id            TEXT      PRIMARY KEY,
    request_hash  TEXT      NOT NULL,
    status        INTEGER   NOT NULL DEFAULT 0,
    body          TEXT      NOT NULL DEFAULT '',
    created_at    DATETIME  NOT NULL
);
