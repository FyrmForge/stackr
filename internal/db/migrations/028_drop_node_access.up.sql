-- Access is per placement now (placements.open_to_all, placement_orgs), so
-- the node-level lists go. orgs.node_id and stacks.node_id were nulled by 027
-- and replaced by placement_id.
DROP TABLE node_orgs;
ALTER TABLE nodes DROP COLUMN open_to_all;
ALTER TABLE join_tokens DROP COLUMN org_ids;
ALTER TABLE join_tokens DROP COLUMN open_to_all;
ALTER TABLE orgs DROP COLUMN node_id;
ALTER TABLE stacks DROP COLUMN node_id;
