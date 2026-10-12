-- exit_ladders: the ordered exit nodes of one org, stack, env or tile.
-- Position 0 is tried first. A scope without rows inherits from the one above
-- it; the node itself is an exit only when it is listed. node_id is not a
-- foreign key (a node row going away is the node leaf's job to clean up).
CREATE TABLE exit_ladders (
    scope_kind  TEXT     NOT NULL CHECK (scope_kind IN ('org', 'stack', 'env', 'tile')),
    scope_id    TEXT     NOT NULL,
    position    INTEGER  NOT NULL CHECK (position >= 0),
    node_id     TEXT     NOT NULL,
    PRIMARY KEY (scope_kind, scope_id, position),
    UNIQUE (scope_kind, scope_id, node_id)
);

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
