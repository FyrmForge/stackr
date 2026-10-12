-- Migration L: the security audit log. Append only: the triggers refuse any
-- change or delete. Actor and target ids are plain text, not foreign keys, so
-- a row outlives the user, key or stack it names.
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
CREATE INDEX security_events_at ON security_events (at);
CREATE INDEX security_events_action ON security_events (action, at);
CREATE INDEX security_events_actor ON security_events (actor_id, at);
CREATE INDEX security_events_target ON security_events (target_kind, target_id, at);

CREATE TRIGGER security_events_no_update BEFORE UPDATE ON security_events
BEGIN SELECT RAISE(ABORT, 'security_events is append only'); END;
CREATE TRIGGER security_events_no_delete BEFORE DELETE ON security_events
BEGIN SELECT RAISE(ABORT, 'security_events is append only'); END;
