-- Node exits: a 'node' scope in exit_ladders (a node's own default exit
-- ladder), and every node tagged egress (the tag now means traffic may leave
-- from the node's IP). SQLite cannot widen a CHECK, so exit_ladders is
-- rebuilt as in 010, with its cascade triggers dropped around it.
DROP TRIGGER exit_ladders_org_cascade;
DROP TRIGGER exit_ladders_stack_cascade;
DROP TRIGGER exit_ladders_env_cascade;
DROP TRIGGER exit_ladders_tile_cascade;

CREATE TABLE exit_ladders_new (
    scope_kind  TEXT     NOT NULL CHECK (scope_kind IN ('org', 'stack', 'env', 'tile', 'node')),
    scope_id    TEXT     NOT NULL,
    position    INTEGER  NOT NULL CHECK (position >= 0),
    node_id     TEXT     NOT NULL,
    PRIMARY KEY (scope_kind, scope_id, position),
    UNIQUE (scope_kind, scope_id, node_id)
);

INSERT INTO exit_ladders_new (scope_kind, scope_id, position, node_id)
    SELECT scope_kind, scope_id, position, node_id FROM exit_ladders;

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
CREATE TRIGGER exit_ladders_node_cascade AFTER DELETE ON nodes BEGIN
    DELETE FROM exit_ladders WHERE scope_kind = 'node' AND scope_id = old.id;
END;

-- A node with no tags may hold the JSON text 'null', which json_insert leaves as is.
UPDATE nodes SET tags = json_insert(CASE json_type(tags) WHEN 'array' THEN tags ELSE '[]' END, '$[#]', 'egress')
WHERE NOT EXISTS (SELECT 1 FROM json_each(nodes.tags) WHERE value = 'egress');
