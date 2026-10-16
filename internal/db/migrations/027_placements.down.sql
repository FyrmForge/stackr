-- Not a full restore: placements and their access lists go. Pins, ingress
-- picks and exit rungs go back to the node their placement stands on, and
-- are dropped when it has none. Node access (node_orgs) was never changed;
-- the storage tag stays (an unknown tag is ignored).
UPDATE orgs SET node_id = (SELECT p.node_id FROM placements p JOIN nodes n ON n.id = p.node_id WHERE p.id = orgs.placement_id)
WHERE placement_id IS NOT NULL;
UPDATE stacks SET node_id = (SELECT p.node_id FROM placements p JOIN nodes n ON n.id = p.node_id WHERE p.id = stacks.placement_id)
WHERE placement_id IS NOT NULL;
UPDATE orgs SET placement_id = NULL;
UPDATE stacks SET placement_id = NULL;

UPDATE ingress_picks SET placement_id = (SELECT node_id FROM placements WHERE id = ingress_picks.placement_id)
WHERE placement_id IN (SELECT id FROM placements);
DELETE FROM ingress_picks WHERE placement_id = '';
UPDATE OR IGNORE exit_ladders SET target_id = (SELECT node_id FROM placements WHERE id = exit_ladders.target_id)
WHERE scope_kind != 'node' AND target_id IN (SELECT id FROM placements);
DELETE FROM exit_ladders WHERE target_id = '' OR (scope_kind != 'node' AND target_id IN (SELECT id FROM placements));

DELETE FROM settings WHERE key = 'server_setup_done_at';

ALTER TABLE exit_ladders RENAME COLUMN target_id TO node_id;
ALTER TABLE ingress_picks RENAME COLUMN placement_id TO node_id;
ALTER TABLE env_moves DROP COLUMN placement_move_id;
ALTER TABLE environments DROP COLUMN placement_id;
ALTER TABLE stacks DROP COLUMN placement_id;
ALTER TABLE orgs DROP COLUMN placement_id;
DROP TABLE placement_moves;
DROP TABLE placement_orgs;
DROP TABLE placements;
