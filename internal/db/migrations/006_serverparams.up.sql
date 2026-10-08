-- Migration F: the server params scope. params.scope_kind gains 'server'
-- (scope_id is the constant "server"); SQLite cannot widen a CHECK, so the
-- table is rebuilt. Nothing references params by a foreign key. The scope
-- triggers on orgs, stacks and environments only name the table in their
-- bodies, and are dropped and recreated around the rename so the rename
-- never has to re-parse them against a missing table.
DROP TRIGGER orgs_scope_cascade;
DROP TRIGGER stacks_scope_cascade;
DROP TRIGGER environments_scope_cascade;

CREATE TABLE params_new (
    id          TEXT      PRIMARY KEY,
    scope_kind  TEXT      NOT NULL CHECK (scope_kind IN ('org', 'stack', 'env', 'server')),
    scope_id    TEXT      NOT NULL,
    collection  TEXT      NOT NULL,
    name        TEXT      NOT NULL,
    kind        TEXT      NOT NULL CHECK (kind IN ('param', 'secret')),
    value       TEXT      NOT NULL, -- encrypted
    created_at  DATETIME  NOT NULL,
    updated_at  DATETIME  NOT NULL,
    UNIQUE (scope_kind, scope_id, collection, name)
);

INSERT INTO params_new (id, scope_kind, scope_id, collection, name, kind, value, created_at, updated_at)
    SELECT id, scope_kind, scope_id, collection, name, kind, value, created_at, updated_at FROM params;

DROP TABLE params;
ALTER TABLE params_new RENAME TO params;

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
