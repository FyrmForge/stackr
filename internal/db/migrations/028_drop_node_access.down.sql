-- Best effort: node access comes back from the placements that stand on each
-- node (closed when any of them is closed). Join tokens come back open to all.
ALTER TABLE stacks ADD COLUMN node_id TEXT REFERENCES nodes (id) ON DELETE SET NULL;
ALTER TABLE orgs   ADD COLUMN node_id TEXT REFERENCES nodes (id) ON DELETE SET NULL;
ALTER TABLE join_tokens ADD COLUMN open_to_all INTEGER NOT NULL DEFAULT 1;
ALTER TABLE join_tokens ADD COLUMN org_ids TEXT NOT NULL DEFAULT '[]';
ALTER TABLE nodes ADD COLUMN open_to_all INTEGER NOT NULL DEFAULT 1;
CREATE TABLE node_orgs (
    node_id TEXT NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    org_id  TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    PRIMARY KEY (node_id, org_id)
);
UPDATE nodes SET open_to_all = 0
WHERE role = 'worker' AND id IN (SELECT node_id FROM placements WHERE open_to_all = 0);
INSERT OR IGNORE INTO node_orgs (node_id, org_id)
SELECT p.node_id, po.org_id FROM placement_orgs po JOIN placements p ON p.id = po.placement_id
WHERE p.open_to_all = 0 AND p.node_id IN (SELECT id FROM nodes);
