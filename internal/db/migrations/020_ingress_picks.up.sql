-- ingress_picks: the ingress node of one org, stack, env or tile. A scope
-- without a row inherits from the one above it, and the controller is the
-- default. node_id is not a foreign key (as in exit_ladders).
CREATE TABLE ingress_picks (
    scope_kind  TEXT  NOT NULL CHECK (scope_kind IN ('org', 'stack', 'env', 'tile')),
    scope_id    TEXT  NOT NULL,
    node_id     TEXT  NOT NULL,
    PRIMARY KEY (scope_kind, scope_id)
);

CREATE INDEX ingress_picks_node ON ingress_picks (node_id);

CREATE TRIGGER ingress_picks_org_cascade AFTER DELETE ON orgs BEGIN
    DELETE FROM ingress_picks WHERE scope_kind = 'org' AND scope_id = old.id;
END;
CREATE TRIGGER ingress_picks_stack_cascade AFTER DELETE ON stacks BEGIN
    DELETE FROM ingress_picks WHERE scope_kind = 'stack' AND scope_id = old.id;
END;
CREATE TRIGGER ingress_picks_env_cascade AFTER DELETE ON environments BEGIN
    DELETE FROM ingress_picks WHERE scope_kind = 'env' AND scope_id = old.id;
END;
CREATE TRIGGER ingress_picks_tile_cascade AFTER DELETE ON tiles BEGIN
    DELETE FROM ingress_picks WHERE scope_kind = 'tile' AND scope_id = old.id;
END;
