-- Rebuild exit_ladders without the 'node' scope (those rows are dropped). The
-- egress tags stay: an absent tag before 024 meant "not an exit", and that
-- cannot be told apart from the tags 024 added.
DROP TRIGGER exit_ladders_node_cascade;
DROP TRIGGER exit_ladders_org_cascade;
DROP TRIGGER exit_ladders_stack_cascade;
DROP TRIGGER exit_ladders_env_cascade;
DROP TRIGGER exit_ladders_tile_cascade;

CREATE TABLE exit_ladders_new (
    scope_kind  TEXT     NOT NULL CHECK (scope_kind IN ('org', 'stack', 'env', 'tile')),
    scope_id    TEXT     NOT NULL,
    position    INTEGER  NOT NULL CHECK (position >= 0),
    node_id     TEXT     NOT NULL,
    PRIMARY KEY (scope_kind, scope_id, position),
    UNIQUE (scope_kind, scope_id, node_id)
);

INSERT INTO exit_ladders_new (scope_kind, scope_id, position, node_id)
    SELECT scope_kind, scope_id, position, node_id FROM exit_ladders WHERE scope_kind <> 'node';

DROP TABLE exit_ladders;
ALTER TABLE exit_ladders_new RENAME TO exit_ladders;

CREATE INDEX exit_ladders_node ON exit_ladders (node_id);

CREATE TRIGGER exit_ladders_org_cascade AFTER DELETE ON orgs BEGIN
    DELETE FROM exit_ladders WHERE scope_kind = 'org' AND scope_id = old.id;
END;
CREATE TRIGGER exit_ladders_stack_cascade AFTER DELETE ON stacks BEGIN
    DELETE FROM exit_ladders WHERE scope_kind = 'stack' AND scope_id = old.id;
END;
CREATE TRIGGER exit_ladders_env_cascade AFTER DELETE ON environments BEGIN
    DELETE FROM exit_ladders WHERE scope_kind = 'env' AND scope_id = old.id;
END;
CREATE TRIGGER exit_ladders_tile_cascade AFTER DELETE ON tiles BEGIN
    DELETE FROM exit_ladders WHERE scope_kind = 'tile' AND scope_id = old.id;
END;
