ALTER TABLE stacks DROP COLUMN node_id;
ALTER TABLE orgs DROP COLUMN node_id;
ALTER TABLE environments DROP COLUMN subnet;
ALTER TABLE environments DROP COLUMN node_id;
DROP INDEX nodes_one_controller;
DROP TABLE nodes;
