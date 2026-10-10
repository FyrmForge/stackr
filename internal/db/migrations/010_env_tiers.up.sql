-- Env tiers: the org's ordered tiers (an env's tier is the one whose slug
-- equals its slug), a lock on every stack env, and three new params scopes:
-- 'tier' (scope_id the tier), 'stack_pr' (the stack) and 'org_pr' (the org).
-- 'stack' is dropped (its rows are deleted). SQLite cannot widen a CHECK,
-- so params is rebuilt as in 006, with the scope triggers dropped around it.
CREATE TABLE tiers (
    id          TEXT      PRIMARY KEY,
    org_id      TEXT      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    slug        TEXT      NOT NULL,
    position    INTEGER   NOT NULL,
    locked      BOOLEAN   NOT NULL DEFAULT 1,
    created_at  DATETIME  NOT NULL,
    UNIQUE (org_id, slug)
);

ALTER TABLE environments ADD COLUMN locked BOOLEAN NOT NULL DEFAULT 1;

DROP TRIGGER orgs_scope_cascade;
DROP TRIGGER stacks_scope_cascade;
DROP TRIGGER environments_scope_cascade;

CREATE TABLE params_new (
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

DELETE FROM params WHERE scope_kind = 'stack';

INSERT INTO params_new (id, scope_kind, scope_id, collection, name, kind, value, created_at, updated_at)
    SELECT id, scope_kind, scope_id, collection, name, kind, value, created_at, updated_at FROM params;

DROP TABLE params;
ALTER TABLE params_new RENAME TO params;

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

CREATE TRIGGER tiers_scope_cascade AFTER DELETE ON tiers BEGIN
    DELETE FROM params WHERE scope_kind = 'tier' AND scope_id = old.id;
END;
