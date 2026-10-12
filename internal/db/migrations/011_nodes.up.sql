-- Nodes: the machines stackr runs envs on. The controller runs the panel,
-- every other node is a worker. range_cidr is the node's /16 of the cluster
-- supernet; tags is a JSON array of capabilities (ingress, egress, builder).
-- The controller row is made at boot by the node leaf, not here.
CREATE TABLE nodes (
    id             TEXT      PRIMARY KEY,
    name           TEXT      NOT NULL UNIQUE,
    slug           TEXT      NOT NULL UNIQUE,
    role           TEXT      NOT NULL CHECK (role IN ('controller', 'worker')),
    tags           TEXT      NOT NULL,
    range_cidr     TEXT      NOT NULL UNIQUE,
    wg_public_key  TEXT      NOT NULL,
    wg_address     TEXT      NOT NULL,
    status         TEXT      NOT NULL,
    created_at     DATETIME  NOT NULL
);

-- One controller, whatever races at boot.
CREATE UNIQUE INDEX nodes_one_controller ON nodes (role) WHERE role = 'controller';

-- environments.node_id is where the env actually runs. SQLite cannot add a
-- NOT NULL column that carries a foreign key, so it is plain text; '' only
-- on rows made before the node leaf (a dev database paves instead).
-- subnet is the env's /24, empty until the node leaf reserves it.
ALTER TABLE environments ADD COLUMN node_id TEXT NOT NULL DEFAULT '';
ALTER TABLE environments ADD COLUMN subnet  TEXT NOT NULL DEFAULT '';

-- Pins decide where new envs go; they never move an existing env.
ALTER TABLE orgs   ADD COLUMN node_id TEXT REFERENCES nodes (id) ON DELETE SET NULL;
ALTER TABLE stacks ADD COLUMN node_id TEXT REFERENCES nodes (id) ON DELETE SET NULL;
