-- Nodes open to every org by default. nodes.open_to_all: any org's owners may
-- pin to the node; node_orgs still lists the picks for when it is turned off.
-- join_tokens carries the add-node choice to the node it makes: open_to_all,
-- or the org ids in org_ids (a JSON array).
ALTER TABLE nodes ADD COLUMN open_to_all INTEGER NOT NULL DEFAULT 1;
ALTER TABLE join_tokens ADD COLUMN open_to_all INTEGER NOT NULL DEFAULT 1;
ALTER TABLE join_tokens ADD COLUMN org_ids TEXT NOT NULL DEFAULT '[]';
