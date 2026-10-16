-- Placements: a named place envs belong to, standing on one node at a time
-- (node_id '' = no node yet). Pins, ingress picks and exit rungs of org,
-- stack, env and tile scopes name placements from here on; 'node' scope rungs
-- keep node ids. placements.node_id has no FK: a forgotten node's placement
-- keeps naming it, so repointing it restores from backups.
CREATE TABLE placements (
    id          TEXT      PRIMARY KEY,
    name        TEXT      NOT NULL UNIQUE,
    node_id     TEXT      NOT NULL DEFAULT '',
    open_to_all INTEGER   NOT NULL DEFAULT 1,
    created_at  DATETIME  NOT NULL
);
CREATE INDEX placements_node ON placements (node_id);

-- The orgs a placement is open to when open_to_all is off.
CREATE TABLE placement_orgs (
    placement_id TEXT NOT NULL REFERENCES placements (id) ON DELETE CASCADE,
    org_id       TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    PRIMARY KEY (placement_id, org_id)
);

-- One repoint of a placement. from_node and to_node have no FK, as in
-- env_moves. The placement keeps from_node until every env is on to_node.
CREATE TABLE placement_moves (
    id           TEXT      PRIMARY KEY,
    placement_id TEXT      NOT NULL REFERENCES placements (id) ON DELETE CASCADE,
    from_node    TEXT      NOT NULL,
    to_node      TEXT      NOT NULL,
    restore      BOOLEAN   NOT NULL,
    state        TEXT      NOT NULL CHECK (state IN ('running', 'failed', 'done')),
    by           TEXT      NOT NULL,
    key_id       TEXT      NOT NULL,
    started_at   DATETIME  NOT NULL,
    finished_at  DATETIME,
    error        TEXT      NOT NULL
);
CREATE INDEX placement_moves_placement ON placement_moves (placement_id, started_at);

-- Pins name a placement. environments.placement_id is where the env belongs;
-- environments.node_id stays where it actually runs.
ALTER TABLE orgs   ADD COLUMN placement_id TEXT REFERENCES placements (id) ON DELETE RESTRICT;
ALTER TABLE stacks ADD COLUMN placement_id TEXT REFERENCES placements (id) ON DELETE RESTRICT;
ALTER TABLE environments ADD COLUMN placement_id TEXT NOT NULL DEFAULT '';
ALTER TABLE env_moves ADD COLUMN placement_move_id TEXT;
ALTER TABLE ingress_picks RENAME COLUMN node_id TO placement_id;
ALTER TABLE exit_ladders RENAME COLUMN node_id TO target_id;

-- 1. default stands on the controller. A fresh database has no controller
-- row yet: the boot path gives default its node then.
INSERT INTO placements (id, name, node_id, open_to_all, created_at)
VALUES (lower(hex(randomblob(16))), 'default',
        COALESCE((SELECT id FROM nodes WHERE role = 'controller'), ''), 1, CURRENT_TIMESTAMP);

-- 2. One placement per worker anything references, named after it, with the
-- node's access copied. Envs on a forgotten node get one too, named after
-- it, so a repoint can restore them; a name already taken is skipped.
INSERT INTO placements (id, name, node_id, open_to_all, created_at)
SELECT lower(hex(randomblob(16))), CASE WHEN n.name = 'default' THEN 'node-default' ELSE n.name END,
       n.id, n.open_to_all, CURRENT_TIMESTAMP
FROM nodes n
WHERE n.role = 'worker' AND (
       n.id IN (SELECT node_id FROM orgs WHERE node_id IS NOT NULL)
    OR n.id IN (SELECT node_id FROM stacks WHERE node_id IS NOT NULL)
    OR n.id IN (SELECT node_id FROM environments)
    OR n.id IN (SELECT placement_id FROM ingress_picks)
    OR n.id IN (SELECT target_id FROM exit_ladders WHERE scope_kind != 'node'));

INSERT OR IGNORE INTO placements (id, name, node_id, open_to_all, created_at)
SELECT lower(hex(randomblob(16))), f.name, f.id, 1, CURRENT_TIMESTAMP
FROM forgotten_nodes f
WHERE f.id IN (SELECT node_id FROM environments) AND f.id NOT IN (SELECT id FROM nodes);

-- node id -> placement id, for the rewrites below.
CREATE TABLE placement_map (node_id TEXT PRIMARY KEY, placement_id TEXT NOT NULL);
INSERT INTO placement_map (node_id, placement_id)
SELECT p.node_id, p.id FROM placements p WHERE p.node_id != '';

INSERT INTO placement_orgs (placement_id, org_id)
SELECT p.id, o.org_id FROM node_orgs o JOIN placements p ON p.node_id = o.node_id
WHERE p.name != 'default';

-- 3. Pins. orgs.node_id and stacks.node_id stay as dead columns until the
-- next squash (D13).
UPDATE orgs SET placement_id = (SELECT placement_id FROM placement_map WHERE node_id = orgs.node_id)
WHERE node_id IS NOT NULL;
UPDATE stacks SET placement_id = (SELECT placement_id FROM placement_map WHERE node_id = stacks.node_id)
WHERE node_id IS NOT NULL;

-- 4. Where each env belongs: its stack's pin, else its org's, when that pin
-- stands on the env's node; else the placement of its node; else default.
UPDATE environments SET placement_id = COALESCE(
    (SELECT p.id FROM stacks s JOIN placements p ON p.id = s.placement_id
     WHERE s.id = environments.stack_id AND p.node_id = environments.node_id),
    (SELECT p.id FROM stacks s JOIN orgs o ON o.id = s.org_id JOIN placements p ON p.id = o.placement_id
     WHERE s.id = environments.stack_id AND p.node_id = environments.node_id),
    (SELECT placement_id FROM placement_map WHERE node_id = environments.node_id),
    (SELECT id FROM placements WHERE name = 'default'));

UPDATE orgs SET node_id = NULL;
UPDATE stacks SET node_id = NULL;

-- 5. Ingress picks and exit rungs name placements. 'own' and node scope stay.
UPDATE ingress_picks SET placement_id = (SELECT m.placement_id FROM placement_map m WHERE m.node_id = ingress_picks.placement_id)
WHERE placement_id IN (SELECT node_id FROM placement_map);
UPDATE exit_ladders SET target_id = (SELECT m.placement_id FROM placement_map m WHERE m.node_id = exit_ladders.target_id)
WHERE scope_kind != 'node' AND target_id IN (SELECT node_id FROM placement_map);

DROP TABLE placement_map;

-- 6. A server that already has orgs is past first-run server setup.
INSERT OR IGNORE INTO settings (key, value)
SELECT 'server_setup_done_at', strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE EXISTS (SELECT 1 FROM orgs);

-- 7. storage: the controller, and every node that holds volumes today.
UPDATE nodes SET tags = json_insert(CASE json_type(tags) WHEN 'array' THEN tags ELSE '[]' END, '$[#]', 'storage')
WHERE NOT EXISTS (SELECT 1 FROM json_each(nodes.tags) WHERE value = 'storage')
  AND (role = 'controller'
    OR id IN (SELECT node_id FROM volumes)
    OR id IN (SELECT e.node_id FROM environments e JOIN volumes v ON v.scope_kind = 'env' AND v.scope_id = e.id));
